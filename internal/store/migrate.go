package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

type VersionConflictError struct {
	Namespace       string
	Table           string
	ExpectedVersion int
	CurrentVersion  int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("version conflict on %s.%s: schema is at version %d, expected %d; re-describe the table and re-plan against the current version", e.Namespace, e.Table, e.CurrentVersion, e.ExpectedVersion)
}

type MigrationPlan struct {
	DryRun              bool                `json:"dry_run"`
	FromVersion         int                 `json:"from_version"`
	ToVersion           int                 `json:"to_version"`
	Table               *schema.TableSchema `json:"table"`
	Operations          []string            `json:"operations"`
	Destructive         []string            `json:"destructive,omitempty"`
	BackfillRows        int64               `json:"backfill_rows"`
	RebuildFulltext     bool                `json:"rebuild_fulltext"`
	FulltextReindexRows int64               `json:"fulltext_reindex_rows"`
	ClearsEmbeddings    bool                `json:"clears_embeddings"`
	EmbedRows           int64               `json:"embed_rows"`

	Expected Incarnation `json:"-"`
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type migrationStep func(ctx context.Context, tx *sql.Tx) error

type migrationWork struct {
	cur              *schema.TableSchema
	steps            []migrationStep
	plan             *MigrationPlan
	rebuildFTSNeeded bool
	vectorizeChanged bool
}

func (s *Store) Migrate(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expected Incarnation) (*schema.TableSchema, error) {
	expectedVersion := int(expected.Version)
	if len(changes) == 0 {
		return nil, invalidf("no changes given")
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	old, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, err
	}
	if err := checkExpectedVersion(nsName, table, expectedVersion, old); err != nil {
		return nil, err
	}
	w, err := planMigration(ctx, tx, nsName, table, old, changes, emb, expectedVersion)
	if err != nil {
		return nil, err
	}
	cur := w.cur

	for _, st := range w.steps {
		if err := st(ctx, tx); err != nil {
			return nil, fmt.Errorf("migration step failed: %w", err)
		}
	}

	if w.rebuildFTSNeeded {
		if err := dropFTS(ctx, tx, table); err != nil {
			return nil, err
		}
		if fts := ftsFields(cur.Fields); len(fts) > 0 {
			if err := createFTS(ctx, tx, table, fts); err != nil {
				return nil, err
			}
		}
	}

	if w.vectorizeChanged {
		newVec := vectorizeField(cur.Fields)
		if newVec != nil {

			modelChanged := old.EmbedSpace != "" && emb.Identity != "" && old.EmbedSpace != emb.Identity
			if old.VectorizeField() != nil || modelChanged {
				if _, err := tx.ExecContext(ctx,
					fmt.Sprintf(`UPDATE %s SET "_embedding" = NULL`, q(table))); err != nil {
					return nil, err
				}
			}
			cur.EmbedDim = 0
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`ALTER TABLE %s ADD COLUMN "_embedding" BLOB`, q(table))); err != nil {
				if !strings.Contains(err.Error(), "duplicate column") {
					return nil, fmt.Errorf("add _embedding column: %w", err)
				}
			}
			for {
				rows, err := tx.QueryContext(ctx,
					fmt.Sprintf(`SELECT id, %s FROM %s WHERE "_embedding" IS NULL AND %s IS NOT NULL AND %s != '' ORDER BY id LIMIT 128`,
						q(newVec.Name), q(table), q(newVec.Name), q(newVec.Name)))
				if err != nil {
					return nil, err
				}
				type pending struct {
					id   int64
					text string
				}
				var batch []pending
				for rows.Next() {
					var p pending
					if err := rows.Scan(&p.id, &p.text); err != nil {
						rows.Close()
						return nil, err
					}
					batch = append(batch, p)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return nil, err
				}
				if len(batch) == 0 {
					break
				}
				texts := make([]string, len(batch))
				for i, p := range batch {
					texts[i] = p.text
				}
				vecs, err := emb.Embed(ctx, texts)
				if err != nil {
					return nil, fmt.Errorf("backfill embedding failed: %w", err)
				}
				if len(vecs) != len(texts) {
					return nil, fmt.Errorf("backfill: embedding provider returned %d vectors for %d texts", len(vecs), len(texts))
				}
				for _, v := range vecs {
					if len(v) == 0 {
						return nil, invalidf("backfill: embedding provider returned a zero-dimensional vector for table %s", table)
					}
					for _, x := range v {
						if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
							return nil, invalidf("backfill: embedding provider returned a non-finite vector component for table %s", table)
						}
					}
					if cur.EmbedDim == 0 {
						cur.EmbedDim = len(v)
					} else if len(v) != cur.EmbedDim {
						return nil, invalidf("embedding provider returned %d-dimensional vectors mid-backfill (expected %d)", len(v), cur.EmbedDim)
					}
				}
				for i, p := range batch {
					if _, err := tx.ExecContext(ctx,
						fmt.Sprintf(`UPDATE %s SET "_embedding" = ? WHERE id = ?`, q(table)),
						schema.EncodeVector(vecs[i]), p.id); err != nil {
						return nil, err
					}
				}
			}
			cur.EmbedSpace = emb.Identity
			if cur.EmbedDim == 0 {
				var dim int
				if err := tx.QueryRowContext(ctx,
					fmt.Sprintf(`SELECT length("_embedding") / 4 FROM %s WHERE "_embedding" IS NOT NULL LIMIT 1`, q(table))).Scan(&dim); err == nil && dim > 0 {
					cur.EmbedDim = dim
				}
			}
		} else if old.VectorizeField() != nil {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE %s SET "_embedding" = NULL`, q(table))); err != nil {
				return nil, err
			}
		}
	}

	if err := saveSchemaTx(ctx, tx, nsName, cur, old.Version, changes); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cur, nil
}

func (s *Store) PlanMigration(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expected Incarnation, scope *RowScope, scopeIncarnation Incarnation) (*MigrationPlan, error) {
	if scope != nil {
		return nil, errScopedPlanUnsupported
	}
	if err := s.guardIncarnation(ctx, nsName, table, scopeIncarnation); err != nil {
		return nil, err
	}
	expectedVersion := int(expected.Version)
	if len(changes) == 0 {
		return nil, invalidf("no changes given")
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	old, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, err
	}
	if err := checkExpectedVersion(nsName, table, expectedVersion, old); err != nil {
		return nil, err
	}
	w, err := planMigration(ctx, tx, nsName, table, old, changes, emb, expectedVersion)
	if err != nil {
		return nil, err
	}
	w.plan.DryRun = true
	return w.plan, nil
}

func checkExpectedVersion(nsName, table string, expected int, old *schema.TableSchema) error {
	if expected < 0 {
		return invalidf("expected_version must be a positive schema version, got %d", expected)
	}
	if expected > 0 && expected != old.Version {
		return &VersionConflictError{
			Namespace:       nsName,
			Table:           table,
			ExpectedVersion: expected,
			CurrentVersion:  old.Version,
		}
	}
	return nil
}

func planMigration(ctx context.Context, db querier, nsName, table string, old *schema.TableSchema, changes []schema.Change, emb Embedder, expectedVersion int) (*migrationWork, error) {
	fields := make([]schema.Field, len(old.Fields))
	copy(fields, old.Fields)
	cur := &schema.TableSchema{Namespace: nsName, Name: table, Version: old.Version, Fields: fields,
		EmbedSpace: old.EmbedSpace, EmbedDim: old.EmbedDim, RowAccess: old.RowAccess, HasOwner: old.HasOwner}

	plan := &MigrationPlan{
		FromVersion: old.Version,
		ToVersion:   old.Version + 1,
		Table:       cur,
		Operations:  []string{},
	}
	w := &migrationWork{cur: cur, plan: plan}

	var rebuildFTSNeeded, vectorizeChanged bool
	var droppedFTSChange bool

	findField := func(name string) (*schema.Field, error) {
		for i := range cur.Fields {
			if cur.Fields[i].Name == name {
				return &cur.Fields[i], nil
			}
		}
		return nil, invalidf("field %q not found", name)
	}

	physicalName := map[string]string{}
	for _, f := range old.Fields {
		physicalName[f.Name] = f.Name
	}

	defaults := map[string]any{}

	for i, ch := range changes {
		if ch.Op != schema.OpAddField && ch.Default != nil {
			return nil, invalidf("changes[%d]: default is only allowed on add_field (op %q has no added field to backfill)", i, ch.Op)
		}
		if (ch.Op == schema.OpSetFulltext || ch.Op == schema.OpSetVectorize) && ch.Value == nil {
			return nil, invalidf("changes[%d]: %s requires an explicit value (true or false)", i, ch.Op)
		}
		if ch.Op != schema.OpSetFulltext && ch.Op != schema.OpSetVectorize && ch.Value != nil {
			return nil, invalidf("changes[%d]: value is only allowed on set_fulltext/set_vectorize (op %q has no flag to set)", i, ch.Op)
		}
		if ch.Op == schema.OpSetEnum && ch.Enum == nil {
			return nil, invalidf("changes[%d]: set_enum requires an explicit enum array (the field's complete new vocabulary; pass an empty array to remove the constraint)", i)
		}
		if ch.Op != schema.OpSetEnum && ch.Enum != nil {
			return nil, invalidf("changes[%d]: enum is only allowed on set_enum (op %q has no enum to set)", i, ch.Op)
		}
		switch ch.Op {
		case schema.OpAddField:
			if ch.Field == nil {
				return nil, invalidf("add_field needs a field object")
			}

			if ch.Field.Default != nil {
				return nil, invalidf("changes[%d]: add_field takes default on the change (\"default\": ...), not inside field; a field default would silently change future inserts", i)
			}
			f := schema.Normalize([]schema.Field{*ch.Field})[0]
			if err := schema.ValidateIdent(f.Name, "field name"); err != nil {
				return nil, invalidf("%s", err)
			}
			for _, ef := range cur.Fields {
				if ef.Name == f.Name {
					return nil, invalidf("field %q already exists", f.Name)
				}
			}
			if len(cur.Fields) >= MaxFieldsPerTable {
				return nil, invalidf("migration would leave %d fields (max %d; ALTERs run in request order, so adds cannot exceed the cap even when later drops reduce the final count)", len(cur.Fields)+1, MaxFieldsPerTable)
			}

			defSQL := ""
			var defVal any
			if ch.Default != nil {
				cv, err := coerceValue(f, ch.Default)
				if err != nil {
					return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
				}

				if fv, isFloat := cv.(float64); isFloat && (math.IsNaN(fv) || math.IsInf(fv, 0)) {
					return nil, invalidf("field %q: default must be a finite number", f.Name)
				}

				if sv, isStr := cv.(string); isStr && strings.ContainsRune(sv, 0) {
					return nil, invalidf("field %q: default must not contain NUL bytes", f.Name)
				}
				defVal = cv
				if f.Required {
					defSQL, err = sqlLiteral(cv)
					if err != nil {
						return nil, err
					}
				}
			}
			if f.Required || ch.Default != nil {
				rowCount, err := countRows(ctx, db, table)
				if err != nil {
					return nil, err
				}
				if f.Required && ch.Default == nil && rowCount > 0 {
					return nil, invalidf("cannot add required field %q to a table with %d existing rows (no backfill value can be supplied); add it nullable instead, or pass a default", f.Name, rowCount)
				}
				if ch.Default != nil {
					plan.BackfillRows += rowCount
				}
			}
			cur.Fields = append(cur.Fields, f)
			physicalName[f.Name] = ""

			defaults[f.Name] = defVal
			if f.Fulltext {
				rebuildFTSNeeded = true
			}
			if f.Vectorize {
				vectorizeChanged = true
			}
			op := fmt.Sprintf("add_field %s (%s", f.Name, f.Type)
			if f.Required {
				op += ", required"
			}
			if f.Fulltext {
				op += ", fulltext"
			}
			if f.Vectorize {
				op += ", vectorize"
			}
			if f.Type == schema.Vector {
				op += fmt.Sprintf(", dim %d", f.Dim)
			}
			if ch.Default != nil {
				op += ", default " + describeValue(ch.Default)
			}
			plan.Operations = append(plan.Operations, op+")")
			w.steps = append(w.steps, func(ctx context.Context, tx *sql.Tx) error {
				ddl := schema.SQLType(f)
				if f.Required {
					ddl += ` NOT NULL`
				}
				if defSQL != "" {
					ddl += ` DEFAULT ` + defSQL
				}
				_, err := tx.ExecContext(ctx,
					fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, q(table), q(f.Name), ddl))
				return err
			})
			if ch.Default != nil && !f.Required {
				w.steps = append(w.steps, func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx,
						fmt.Sprintf(`UPDATE %s SET %s = ?`, q(table), q(f.Name)), defVal)
					return err
				})
			}
		case schema.OpRenameField:
			if err := schema.ValidateIdent(ch.To, "new field name"); err != nil {
				return nil, invalidf("%s", err)
			}
			f, err := findField(ch.From)
			if err != nil {
				return nil, err
			}
			oldName := f.Name
			if ch.To != ch.From {
				for _, ef := range cur.Fields {
					if ef.Name == ch.To {
						return nil, invalidf("field %q already exists", ch.To)
					}
				}
			}
			f.Name = ch.To
			if ch.To != ch.From {
				physicalName[ch.To] = physicalName[oldName]
				delete(physicalName, oldName)
				if d, ok := defaults[oldName]; ok {
					defaults[ch.To] = d
					delete(defaults, oldName)
				}
			}
			if f.Fulltext {
				rebuildFTSNeeded = true
			}
			plan.Operations = append(plan.Operations, fmt.Sprintf("rename_field %s -> %s", ch.From, ch.To))
			plan.Destructive = append(plan.Destructive, fmt.Sprintf("rename_field %s -> %s (queries and writers using the old name break)", ch.From, ch.To))
			w.steps = append(w.steps, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, q(table), q(oldName), q(ch.To)))
				return err
			})
		case schema.OpDropField:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if f.Vectorize {
				vectorizeChanged = true
			}
			if f.Fulltext {
				rebuildFTSNeeded = true
				droppedFTSChange = true
			}
			idx := -1
			for i := range cur.Fields {
				if cur.Fields[i].Name == ch.Name {
					idx = i
					break
				}
			}
			cur.Fields = append(cur.Fields[:idx], cur.Fields[idx+1:]...)
			delete(physicalName, ch.Name)
			delete(defaults, ch.Name)
			plan.Operations = append(plan.Operations, fmt.Sprintf("drop_field %s", ch.Name))
			plan.Destructive = append(plan.Destructive, fmt.Sprintf("drop_field %s (the column and its data are removed permanently)", ch.Name))
			w.steps = append(w.steps, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, q(table), q(ch.Name)))
				return err
			})
		case schema.OpSetFulltext:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if *ch.Value && f.Type != schema.String && f.Type != schema.Text {
				return nil, invalidf("field %q: fulltext is only allowed on string or text fields", f.Name)
			}
			if f.Fulltext != *ch.Value {
				f.Fulltext = *ch.Value
				rebuildFTSNeeded = true
			} else if *ch.Value {

				rebuildFTSNeeded = true
			}
			plan.Operations = append(plan.Operations, fmt.Sprintf("set_fulltext %s = %t", ch.Name, *ch.Value))
		case schema.OpSetVectorize:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if *ch.Value {
				if f.Type != schema.String && f.Type != schema.Text {
					return nil, invalidf("field %q: vectorize is only allowed on string or text fields", f.Name)
				}
				for i := range cur.Fields {
					if cur.Fields[i].Vectorize && &cur.Fields[i] != f {
						return nil, invalidf("at most one vectorized field per table (already %q)", cur.Fields[i].Name)
					}
				}
			}
			if f.Vectorize != *ch.Value {
				f.Vectorize = *ch.Value
				vectorizeChanged = true
			}
			plan.Operations = append(plan.Operations, fmt.Sprintf("set_vectorize %s = %t", ch.Name, *ch.Value))
		case schema.OpSetEnum:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if f.Type != schema.String {
				return nil, invalidf("field %q: enum is only allowed on string fields (this field has type %s)", f.Name, f.Type)
			}

			vals := make([]string, len(*ch.Enum))
			copy(vals, *ch.Enum)
			if len(vals) > 0 {
				if err := schema.ValidateEnum(f.Name, vals); err != nil {
					return nil, invalidf("%s", err)
				}
			}

			if len(vals) > 0 {
				if phys := physicalName[f.Name]; phys != "" {
					rows, err := db.QueryContext(ctx,
						fmt.Sprintf(`SELECT %s, count(*) FROM %s WHERE %s IS NOT NULL GROUP BY %s`, q(phys), q(table), q(phys), q(phys)))
					if err != nil {
						return nil, err
					}
					type usage struct {
						val string
						n   int64
					}
					var inUse []usage
					for rows.Next() {
						var u usage
						if err := rows.Scan(&u.val, &u.n); err != nil {
							rows.Close()
							return nil, err
						}
						if !schema.EnumAllows(vals, u.val) {
							inUse = append(inUse, u)
						}
					}
					if err := rows.Err(); err != nil {
						rows.Close()
						return nil, err
					}
					rows.Close()
					if len(inUse) > 0 {
						parts := make([]string, len(inUse))
						for k, u := range inUse {
							parts[k] = fmt.Sprintf("%q is stored by %d rows", u.val, u.n)
						}
						return nil, invalidf("field %q: cannot apply this enum — %s; update those rows to a kept value first (update with set %s = ...), or keep the values in the enum", f.Name, strings.Join(parts, ", "), f.Name)
					}
				}
			}

			if f.Default != nil {
				if s, ok := storedString(f.Default); ok && !schema.EnumAllows(vals, s) {
					return nil, invalidf("field %q: the declared default %q is not in the new enum (%s); keep the value, or pick a default among the allowed values", f.Name, s, strings.Join(vals, ", "))
				}
			}

			if dv := defaults[f.Name]; dv != nil {
				if s, ok := dv.(string); ok && !schema.EnumAllows(vals, s) {
					return nil, invalidf("field %q: the add_field backfill default %q is not in the new enum (%s); keep the value, or pick a backfill among the allowed values", f.Name, s, strings.Join(vals, ", "))
				}
			}

			if len(vals) > 0 {
				f.Enum = vals
			} else {
				f.Enum = nil
			}
			plan.Operations = append(plan.Operations, "set_enum "+ch.Name+" = "+describeValue(vals))
		default:
			return nil, invalidf("unknown migration op %q (valid: add_field, rename_field, drop_field, set_fulltext, set_vectorize, set_enum)", ch.Op)
		}
	}
	if len(plan.Destructive) > 0 && expectedVersion == 0 {
		return nil, invalidf("destructive changes require expected_version (from describe_table) so a stale plan cannot run against a schema that moved on: %s", strings.Join(plan.Destructive, "; "))
	}
	if err := schema.ValidateForMigration(cur.Fields, old.Fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(ftsFields(cur.Fields)) == 0 && len(ftsFields(old.Fields)) > 0 {
		rebuildFTSNeeded = true
		droppedFTSChange = true
	}
	_ = droppedFTSChange

	plan.RebuildFulltext = rebuildFTSNeeded
	if rebuildFTSNeeded {

		var preds []string
		for _, f := range ftsFields(cur.Fields) {
			if phys := physicalName[f.Name]; phys == "" {
				if defaults[f.Name] != nil {
					preds = append(preds, `1`)
				}
				continue
			} else {
				preds = append(preds, fmt.Sprintf(`%s IS NOT NULL`, q(phys)))
			}
		}
		if len(preds) > 0 {
			var n int64
			if err := db.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s`, q(table), strings.Join(preds, ` OR `))).Scan(&n); err != nil {
				return nil, err
			}
			plan.FulltextReindexRows = n
		}
	}

	if vectorizeChanged {
		newVec := vectorizeField(cur.Fields)
		if newVec != nil {
			if emb.Embed == nil {
				return nil, invalidf("vectorize requires an embedding provider (set DOLMEN_EMBED_PROVIDER=local or openai)")
			}
			if emb.Identity == "" {
				return nil, invalidf("vectorize requires an embedding provider with a reported identity so backfilled rows are attributable to an embedding space; the active provider reports none — an operator must set DOLMEN_EMBED_PROVIDER (local or openai, plus credentials such as DOLMEN_EMBED_API_KEY for openai) and restart the server")
			}
			modelChanged := cur.EmbedSpace != "" && emb.Identity != "" && cur.EmbedSpace != emb.Identity
			plan.ClearsEmbeddings = old.VectorizeField() != nil || modelChanged

			if phys := physicalName[newVec.Name]; phys == "" {
				if s, ok := defaults[newVec.Name].(string); ok && s != "" {
					n, err := countRows(ctx, db, table)
					if err != nil {
						return nil, err
					}
					plan.EmbedRows = n
				}
			} else {
				var n int64
				if err := db.QueryRowContext(ctx,
					fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s IS NOT NULL AND %s != ''`, q(table), q(phys), q(phys))).Scan(&n); err != nil {
					return nil, err
				}
				plan.EmbedRows = n
			}

			cur.EmbedSpace = emb.Identity
			cur.EmbedDim = 0
		} else if old.VectorizeField() != nil {
			plan.ClearsEmbeddings = true
		}
	}

	w.rebuildFTSNeeded = rebuildFTSNeeded
	w.vectorizeChanged = vectorizeChanged
	cur.Version = old.Version + 1
	return w, nil
}

func countRows(ctx context.Context, db querier, table string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, q(table))).Scan(&n)
	return n, err
}

func sqlLiteral(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return `'` + strings.ReplaceAll(x, `'`, `''`) + `'`, nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case []byte:
		return `X'` + hex.EncodeToString(x) + `'`, nil
	default:
		return "", invalidf("unsupported default value type %T", v)
	}
}

func describeValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
