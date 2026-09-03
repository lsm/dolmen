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

// VersionConflictError reports a migrate whose expected_version precondition
// failed: the table's schema already moved past the version the caller planned
// against. Re-describe the table and re-plan on the current version.
type VersionConflictError struct {
	Namespace       string
	Table           string
	ExpectedVersion int
	CurrentVersion  int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("version conflict on %s.%s: schema is at version %d, expected %d; re-describe the table and re-plan against the current version", e.Namespace, e.Table, e.CurrentVersion, e.ExpectedVersion)
}

// MigrationPlan is the prospective outcome of a migration: the schema the table
// would have, which changes destroy data or contracts, and the row-level work
// (default backfills, index rebuilds, embedding calls) applying it would do.
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
}

// querier is the read surface both planning contexts offer: a write
// transaction when applying (so plans are built on the locked, version-checked
// schema) and the read-only connection when dry-running.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type migrationStep func(ctx context.Context, tx *sql.Tx) error

// migrationWork carries everything Migrate executes plus everything
// PlanMigration reports; both are produced by the same builder so dry-run and
// apply can never disagree about what a change list does.
type migrationWork struct {
	cur              *schema.TableSchema
	steps            []migrationStep
	plan             *MigrationPlan
	rebuildFTSNeeded bool
	vectorizeChanged bool
}

func (s *Store) Migrate(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expectedVersion int) (*schema.TableSchema, error) {
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
			modelChanged := cur.EmbedSpace != "" && emb.Identity != "" && cur.EmbedSpace != emb.Identity
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

// PlanMigration validates a change list and reports what applying it would do,
// with zero side effects: it runs on the read-only connection, writes nothing,
// and never calls the embedding provider. It applies the same
// expected_version precondition as Migrate so a stale plan fails the preview
// instead of the apply.
func (s *Store) PlanMigration(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expectedVersion int) (*MigrationPlan, error) {
	if len(changes) == 0 {
		return nil, invalidf("no changes given")
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	old, err := loadSchema(ctx, n.ro, nsName, table)
	if err != nil {
		return nil, err
	}
	if err := checkExpectedVersion(nsName, table, expectedVersion, old); err != nil {
		return nil, err
	}
	w, err := planMigration(ctx, n.ro, nsName, table, old, changes, emb, expectedVersion)
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

// planMigration builds the prospective schema, the executable DDL steps, and
// the plan summary for a change list. It is the single validation path shared
// by dry-run (querier = read-only connection) and apply (querier = the write
// transaction, after the version check).
func planMigration(ctx context.Context, db querier, nsName, table string, old *schema.TableSchema, changes []schema.Change, emb Embedder, expectedVersion int) (*migrationWork, error) {
	fields := make([]schema.Field, len(old.Fields))
	copy(fields, old.Fields)
	cur := &schema.TableSchema{Namespace: nsName, Name: table, Version: old.Version, Fields: fields, EmbedSpace: old.EmbedSpace, EmbedDim: old.EmbedDim}

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

	addedFields := map[string]any{}
	// renamedFrom maps a field's prospective name back to the physical column
	// the database still has: estimate queries run before any DDL step, so they
	// must address current column names, not names this migration creates.
	renamedFrom := map[string]string{}

	for i, ch := range changes {
		if ch.Op != schema.OpAddField && ch.Default != nil {
			return nil, invalidf("changes[%d]: default is only allowed on add_field (op %q has no added field to backfill)", i, ch.Op)
		}
		switch ch.Op {
		case schema.OpAddField:
			if ch.Field == nil {
				return nil, invalidf("add_field needs a field object")
			}
			f := schema.Normalize([]schema.Field{*ch.Field})[0]
			if !schema.ValidIdent(f.Name) {
				return nil, invalidf("invalid field name %q", f.Name)
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
			if ch.Default != nil {
				cv, err := coerceValue(f, ch.Default)
				if err != nil {
					return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
				}
				defSQL, err = sqlLiteral(cv)
				if err != nil {
					return nil, err
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
			addedFields[f.Name] = ch.Default
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
		case schema.OpRenameField:
			if !schema.ValidIdent(ch.To) {
				return nil, invalidf("invalid new field name %q", ch.To)
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
			renamedFrom[ch.To] = oldName
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
			if ch.Value && f.Type != schema.String && f.Type != schema.Text {
				return nil, invalidf("field %q: fulltext is only allowed on string or text fields", f.Name)
			}
			if f.Fulltext != ch.Value {
				f.Fulltext = ch.Value
				rebuildFTSNeeded = true
			}
			plan.Operations = append(plan.Operations, fmt.Sprintf("set_fulltext %s = %t", ch.Name, ch.Value))
		case schema.OpSetVectorize:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if ch.Value {
				if f.Type != schema.String && f.Type != schema.Text {
					return nil, invalidf("field %q: vectorize is only allowed on string or text fields", f.Name)
				}
				for i := range cur.Fields {
					if cur.Fields[i].Vectorize && &cur.Fields[i] != f {
						return nil, invalidf("at most one vectorized field per table (already %q)", cur.Fields[i].Name)
					}
				}
			}
			if f.Vectorize != ch.Value {
				f.Vectorize = ch.Value
				vectorizeChanged = true
			}
			plan.Operations = append(plan.Operations, fmt.Sprintf("set_vectorize %s = %t", ch.Name, ch.Value))
		default:
			return nil, invalidf("unknown migration op %q (valid: add_field, rename_field, drop_field, set_fulltext, set_vectorize)", ch.Op)
		}
	}
	if len(plan.Destructive) > 0 && expectedVersion == 0 {
		return nil, invalidf("destructive changes require expected_version (from describe_table) so a stale plan cannot run against a schema that moved on: %s", strings.Join(plan.Destructive, "; "))
	}
	if err := schema.Validate(cur.Fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(ftsFields(cur.Fields)) == 0 && len(ftsFields(old.Fields)) > 0 {
		rebuildFTSNeeded = true
		droppedFTSChange = true
	}
	_ = droppedFTSChange

	plan.RebuildFulltext = rebuildFTSNeeded
	if rebuildFTSNeeded {
		n, err := countRows(ctx, db, table)
		if err != nil {
			return nil, err
		}
		plan.FulltextReindexRows = n
	}

	if vectorizeChanged {
		newVec := vectorizeField(cur.Fields)
		if newVec != nil {
			if emb.Embed == nil {
				return nil, invalidf("vectorize requires an embedding provider (set DOLMEN_EMBED_PROVIDER)")
			}
			if emb.Identity == "" {
				return nil, invalidf("vectorize requires an embedding provider with a reported identity so backfilled rows are attributable to an embedding space")
			}
			modelChanged := cur.EmbedSpace != "" && emb.Identity != "" && cur.EmbedSpace != emb.Identity
			plan.ClearsEmbeddings = old.VectorizeField() != nil || modelChanged
			// Every enable path re-embeds all rows carrying non-empty text:
			// either there is no _embedding column yet, or the column is being
			// cleared (field or model switch) — so count the texts, not the
			// currently-unembedded rows. Resolve the prospective field name to
			// the column the database has now: a field added by this migration
			// has no stored texts yet (only its non-empty default embeds), and
			// a renamed one lives under its pre-migration name until the DDL
			// steps run.
			physName := newVec.Name
			for older, ok := renamedFrom[physName]; ok; older, ok = renamedFrom[physName] {
				physName = older
			}
			if def, added := addedFields[physName]; added {
				if s, ok := def.(string); ok && s != "" {
					n, err := countRows(ctx, db, table)
					if err != nil {
						return nil, err
					}
					plan.EmbedRows = n
				}
			} else {
				var n int64
				if err := db.QueryRowContext(ctx,
					fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s IS NOT NULL AND %s != ''`, q(table), q(physName), q(physName))).Scan(&n); err != nil {
					return nil, err
				}
				plan.EmbedRows = n
			}
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

// sqlLiteral renders a coerced default as a constant SQL literal for
// ALTER TABLE ... DEFAULT — the one place dolmen interpolates values into DDL,
// so every branch must stay a literal SQLite accepts (never an expression).
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

// describeValue renders a default for the plan's operation list.
func describeValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
