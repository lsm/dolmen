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

	ExpectedIncarnation string `json:"expected_incarnation,omitempty"`

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

func (s *Store) Migrate(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expected Incarnation) (_ *schema.TableSchema, err error) {
	ctx, span := s.tr.Op(ctx, "MIGRATE", nsName, table)
	defer func() { s.tr.End(span, err) }()
	expectedVersion := int(expected.Version)
	if len(changes) == 0 {
		return nil, invalidf("no changes given")
	}
	if err := s.requireSecretKey(addedFields(changes)); err != nil {
		return nil, err
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	defer n.unpin()
	ctx, tx, txSpan, err := s.beginWrite(ctx, n)
	if err != nil {
		return nil, err
	}
	defer s.endWrite(tx, txSpan)

	if err := checkBoundLifetime(ctx, tx, nsName, table, expected); err != nil {
		return nil, err
	}
	old, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, err
	}
	if err := checkExpectedVersion(nsName, table, expectedVersion, old); err != nil {
		return nil, err
	}
	w, err := planMigration(ctx, tx, nsName, table, old, changes, emb, expectedVersion, nil)
	if err != nil {
		return nil, err
	}
	cur := w.cur

	for i, st := range w.steps {
		if err := s.migrateStep(ctx, "change", i, func(ctx context.Context) error { return st(ctx, tx) }); err != nil {
			return nil, fmt.Errorf("migration step failed: %w", err)
		}
	}

	if w.rebuildFTSNeeded {
		if err := s.migrateStep(ctx, "fts_rebuild", len(w.steps), func(ctx context.Context) error {
			if err := dropFTS(ctx, tx, table); err != nil {
				return err
			}
			if fts := ftsFields(cur.Fields); len(fts) > 0 {
				return createFTS(ctx, tx, table, fts)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	endBackfill := func(error) {}
	defer func() { endBackfill(err) }()
	if w.vectorizeChanged {
		var bctx context.Context
		bctx, endBackfill = s.migrateStepSpan(ctx, "embedding_backfill", len(w.steps)+1)
		ctx := bctx
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

	endBackfill(nil)
	if err := saveSchemaTx(ctx, tx, nsName, cur, old.Version, changes); err != nil {
		return nil, err
	}
	if err := commitWrite(tx, txSpan); err != nil {
		return nil, err
	}
	return cur, nil
}

func (s *Store) PlanMigration(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expected Incarnation, scope *RowScope, scopeIncarnation Incarnation) (*MigrationPlan, error) {
	expectedVersion := int(expected.Version)
	if len(changes) == 0 {
		return nil, invalidf("no changes given")
	}
	if err := s.requireSecretKey(addedFields(changes)); err != nil {
		return nil, err
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	defer n.unpin()
	tx, err := n.ro.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := checkScopeIncarnation(ctx, tx, nsName, table, scopeIncarnation); err != nil {
		return nil, err
	}
	old, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, err
	}
	if err := scopeUsable(scope, old); err != nil {
		return nil, err
	}
	if err := checkExpectedVersion(nsName, table, expectedVersion, old); err != nil {
		return nil, err
	}
	w, err := planMigration(ctx, tx, nsName, table, old, changes, emb, expectedVersion, scope)
	if err != nil {
		return nil, err
	}
	w.plan.DryRun = true
	gen, err := readNSGen(ctx, tx)
	if err != nil {
		return nil, err
	}
	dropGen, err := tableGen(ctx, tx, table)
	if err != nil {
		return nil, err
	}
	w.plan.Expected = Incarnation{NsGen: gen, Table: table, Version: int64(old.Version), DropGen: dropGen}
	w.plan.ExpectedIncarnation = EncodeIncarnation(w.plan.Expected)
	return w.plan, nil
}

func checkBoundLifetime(ctx context.Context, tx rowQuerier, nsName, table string, want Incarnation) error {
	bound := want.NsGen != [16]byte{} || want.Table != "" || want.DropGen != 0
	if !bound {
		return nil
	}
	replaced := fmt.Errorf("%w: table %s.%s was replaced; describe the current table", ErrNotFound, nsName, table)
	if want.Table != "" && want.Table != table {
		return replaced
	}
	gen, err := readNSGen(ctx, tx)
	if err != nil {
		return err
	}
	dropGen, err := tableGen(ctx, tx, table)
	if err != nil {
		return err
	}
	if want.NsGen != [16]byte{} && want.NsGen != gen {
		return replaced
	}
	if want.DropGen != dropGen {
		return replaced
	}
	return nil
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

func planMigration(ctx context.Context, db querier, nsName, table string, old *schema.TableSchema, changes []schema.Change, emb Embedder, expectedVersion int, scope *RowScope) (*migrationWork, error) {
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
		if schema.TakesValue(ch.Op) && ch.Value == nil {
			return nil, invalidf("changes[%d]: %s requires an explicit value (true or false)", i, ch.Op)
		}
		if !schema.TakesValue(ch.Op) && ch.Value != nil {
			return nil, invalidf("changes[%d]: value is only allowed on set_fulltext/set_vectorize/set_row_access (op %q has no flag to set)", i, ch.Op)
		}
		if cur.HasOwner {
			targets := []string{ch.Name, ch.To}
			if ch.Field != nil {
				targets = append(targets, ch.Field.Name)
			}
			for _, t := range targets {
				if schema.ReservedWithOwner(strings.ToLower(strings.TrimSpace(t))) {
					return nil, invalidf("%q is the implicit owner column on this table, which carries it because row_access was declared; the name stays reserved while the column exists, even with row_access turned off, so pick another name such as %q", schema.OwnerColumn, "owner_name")
				}
			}
		}
		if ch.Op == schema.OpSetEnum && ch.Enum == nil {
			return nil, invalidf("changes[%d]: set_enum requires an explicit enum array (the field's complete new vocabulary; pass an empty array to remove the constraint)", i)
		}
		if ch.Op != schema.OpSetEnum && ch.Enum != nil {
			return nil, invalidf("changes[%d]: enum is only allowed on set_enum (op %q has no enum to set)", i, ch.Op)
		}
		if err := ShapeChangeArgs(i, ch); err != nil {
			return nil, err
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
			if f.Type == schema.Secret && ch.Default != nil {
				return nil, invalidf("%s", schema.SecretRefusal(f.Name, "default"))
			}
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
					if scope != nil {
						return nil, invalidf("cannot add required field %q to a table that already holds rows (no backfill value can be supplied); add it nullable instead, or pass a default. How many rows is reported only to a caller holding read on the table", f.Name)
					}
					return nil, invalidf("cannot add required field %q to a table with %d existing rows (no backfill value can be supplied); add it nullable instead, or pass a default", f.Name, rowCount)
				}
				if ch.Default != nil {
					visible, err := visibleCount(ctx, db, table, scope)
					if err != nil {
						return nil, err
					}
					plan.BackfillRows += visible
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
			if *ch.Value && f.Type == schema.Secret {
				return nil, invalidf("%s", schema.SecretRefusal(f.Name, "fulltext"))
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
				if f.Type == schema.Secret {
					return nil, invalidf("%s", schema.SecretRefusal(f.Name, "vectorize"))
				}
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
			if f.Type == schema.Secret && len(*ch.Enum) > 0 {
				return nil, invalidf("%s", schema.SecretRefusal(f.Name, "enum"))
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
						if scope != nil {
							return nil, invalidf("field %q: cannot apply this enum — rows hold values it does not allow; update those rows to a kept value first (update with set %s = ...), or keep the values in the enum. Which values, and how many rows, is reported only to a caller holding read on the table", f.Name, f.Name)
						}
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

		case schema.OpSetShape:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			shape := *ch.Shape
			if err := ShapeTarget(f, shape); err != nil {
				return nil, err
			}
			if shape != "" {
				if phys := physicalName[f.Name]; phys != "" {
					rows, err := db.QueryContext(ctx,
						fmt.Sprintf(`SELECT id, %s FROM %s WHERE %s IS NOT NULL ORDER BY id`, q(phys), q(table), q(phys)))
					if err != nil {
						return nil, err
					}
					var violating int64
					var sample []int64
					for rows.Next() {
						var id int64
						var stored string
						if err := rows.Scan(&id, &stored); err != nil {
							rows.Close()
							return nil, err
						}
						if !ShapeFits(shape, stored) {
							violating++
							if len(sample) < 5 {
								sample = append(sample, id)
							}
						}
					}
					if err := rows.Err(); err != nil {
						rows.Close()
						return nil, err
					}
					rows.Close()
					if violating > 0 {
						return nil, ShapeRowsRefusal(f.Name, shape, violating, sample, scope != nil)
					}
				}
				if err := ShapeDefaults(f, shape, defaults[f.Name]); err != nil {
					return nil, err
				}
			}
			f.Shape = shape
			plan.Operations = append(plan.Operations, ShapeOperation(ch.Name, shape))

		case schema.OpSetRowAccess:
			if ch.Value == nil {
				return nil, invalidf("set_row_access requires value: true restricts rows to the principal who wrote them, false stops the filtering")
			}
			if *ch.Value {
				if cur.RowAccess == schema.RowAccessOwn {
					plan.Operations = append(plan.Operations, "set_row_access true (already enabled)")
					break
				}
				if err := ValidateOwnerCollision(cur.Fields); err != nil {
					return nil, err
				}
				var rows int64
				if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, q(table))).Scan(&rows); err != nil {
					return nil, err
				}
				if rows > 0 {
					if scope != nil {
						return nil, invalidf("table %s already holds rows, so row_access cannot be enabled on it: no operation can write another principal's rows as that principal, so there is no honest way to assign owners to what is already there; create a new table with row_access and replay each owner's rows under their own identity, letting the server stamp them. How many rows is reported only to a caller holding read on the table", table)
					}
					return nil, invalidf("table %s already holds %d rows, so row_access cannot be enabled on it: no operation can write another principal's rows as that principal, so there is no honest way to assign owners to what is already there; create a new table with row_access and replay each owner's rows under their own identity, letting the server stamp them", table, rows)
				}
				if !cur.HasOwner {
					w.steps = append(w.steps, func(ctx context.Context, tx *sql.Tx) error {
						_, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s TEXT`, q(table), q(schema.OwnerColumn)))
						return err
					})
				}
				w.steps = append(w.steps, func(ctx context.Context, tx *sql.Tx) error {
					return installRowCount(ctx, tx, table, true)
				})
				cur.RowAccess = schema.RowAccessOwn
				cur.HasOwner = true
				plan.Operations = append(plan.Operations, "set_row_access true")
				break
			}
			if cur.RowAccess == "" {
				plan.Operations = append(plan.Operations, "set_row_access false (already off)")
				break
			}
			cur.RowAccess = ""
			plan.Operations = append(plan.Operations, "set_row_access false (the owner column and its values are kept)")

		default:
			return nil, invalidf("unknown migration op %q (valid: add_field, rename_field, drop_field, set_fulltext, set_vectorize, set_enum, set_shape, set_row_access)", ch.Op)
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
			vis, visArgs := visiblePredicate(scope)
			if err := db.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT count(*) FROM %s WHERE (%s)%s`, q(table), strings.Join(preds, ` OR `), vis), visArgs...).Scan(&n); err != nil {
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
					n, err := visibleCount(ctx, db, table, scope)
					if err != nil {
						return nil, err
					}
					plan.EmbedRows = n
				}
			} else {
				var n int64
				vis, visArgs := visiblePredicate(scope)
				if err := db.QueryRowContext(ctx,
					fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s IS NOT NULL AND %s != ''%s`, q(table), q(phys), q(phys), vis), visArgs...).Scan(&n); err != nil {
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

func visibleCount(ctx context.Context, db querier, table string, scope *RowScope) (int64, error) {
	clause, args := scopeClause(scope, "")
	if clause == "" {
		return countRows(ctx, db, table)
	}
	var n int64
	err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s`, q(table), clause), args...).Scan(&n)
	return n, err
}

func visiblePredicate(scope *RowScope) (string, []any) {
	clause, args := scopeClause(scope, "")
	if clause == "" {
		return "", nil
	}
	return ` AND (` + clause + `)`, args
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

func addedFields(changes []schema.Change) []schema.Field {
	var out []schema.Field
	for _, ch := range changes {
		if ch.Op == schema.OpAddField && ch.Field != nil {
			out = append(out, *ch.Field)
		}
	}
	return out
}
