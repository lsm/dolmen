package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

var errMigrationRetry = errors.New("dolmen: migration lost a race with concurrent writes")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", store.ErrInvalid, fmt.Sprintf(format, args...))
}

type migrationStep struct {
	sql  string
	args []any
}

type embedWork struct {
	clear     bool
	addColumn bool
	source    string
	target    string
	constant  string
	hasSource bool
}

type migrationWork struct {
	cur        *schema.TableSchema
	columns    map[string]string
	plan       *store.MigrationPlan
	steps      []migrationStep
	rebuildFTS bool
	embed      embedWork
	embedding  bool
}

func pgLiteral(f schema.Field, v any) (string, error) {
	switch f.Type {
	case schema.Number:
		switch x := v.(type) {
		case int64:
			return strconv.FormatInt(x, 10), nil
		case float64:
			return strconv.FormatFloat(x, 'g', -1, 64), nil
		}
	case schema.Boolean:
		switch x := v.(type) {
		case int64:
			if x != 0 {
				return "true", nil
			}
			return "false", nil
		case bool:
			if x {
				return "true", nil
			}
			return "false", nil
		}
	case schema.Vector:
		if b, ok := v.([]byte); ok {
			return `'\x` + hex.EncodeToString(b) + `'::bytea`, nil
		}
	default:
		if s, ok := v.(string); ok {
			return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`, nil
		}
	}
	return "", invalidf("unsupported default value type %T", v)
}

func pgWriteValue(f schema.Field, v any) any {
	if f.Type == schema.Boolean {
		if x, ok := v.(int64); ok {
			return x != 0
		}
	}
	if f.Type == schema.Number && v != nil {
		return fmt.Sprint(v)
	}
	return v
}

func (s *Store) countTableRows(ctx context.Context, tx pgx.Tx, table string) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	return n, err
}

func (s *Store) planMigration(ctx context.Context, tx pgx.Tx, n namespace, state tableState, changes []schema.Change, emb store.Embedder, expectedVersion int) (*migrationWork, error) {
	old := state.schema
	table := ident(n.physical, state.physical)

	fields := make([]schema.Field, len(old.Fields))
	copy(fields, old.Fields)
	cur := &schema.TableSchema{Namespace: old.Namespace, Name: old.Name, Version: old.Version, Fields: fields, EmbedSpace: old.EmbedSpace, EmbedDim: old.EmbedDim,
		RowAccess: old.RowAccess, HasOwner: old.HasOwner}

	plan := &store.MigrationPlan{FromVersion: old.Version, ToVersion: old.Version + 1, Table: cur, Operations: []string{}}
	w := &migrationWork{cur: cur, plan: plan}

	namer := newColumnNamer(state.columns)
	onDisk := map[string]bool{}
	source := map[string]string{}
	for _, f := range old.Fields {
		onDisk[f.Name] = true
		source[f.Name] = state.columns[f.Name]
	}

	var rebuildFTSNeeded, vectorizeChanged bool

	findField := func(name string) (*schema.Field, error) {
		for i := range cur.Fields {
			if cur.Fields[i].Name == name {
				return &cur.Fields[i], nil
			}
		}
		return nil, invalidf("field %q not found", name)
	}

	defaults := map[string]any{}

	for i, ch := range changes {
		if ch.Op != schema.OpAddField && ch.Default != nil {
			return nil, invalidf("changes[%d]: default is only allowed on add_field (op %q has no added field to backfill)", i, ch.Op)
		}
		if (ch.Op == schema.OpSetFulltext || ch.Op == schema.OpSetVectorize) && ch.Value == nil {
			return nil, invalidf("changes[%d]: %s requires an explicit value (true or false)", i, ch.Op)
		}
		if ch.Op != schema.OpSetFulltext && ch.Op != schema.OpSetVectorize && ch.Op != schema.OpSetRowAccess && ch.Value != nil {
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
			if len(cur.Fields) >= store.MaxFieldsPerTable {
				return nil, invalidf("migration would leave %d fields (max %d; ALTERs run in request order, so adds cannot exceed the cap even when later drops reduce the final count)", len(cur.Fields)+1, store.MaxFieldsPerTable)
			}

			defSQL := ""
			var defVal any
			if ch.Default != nil {
				cv, err := value.Coerce(f, ch.Default)
				if err != nil {
					return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
				}
				if fv, isFloat := cv.(float64); isFloat && (math.IsNaN(fv) || math.IsInf(fv, 0)) {
					return nil, invalidf("field %q: default must be a finite number", f.Name)
				}
				if sv, isStr := cv.(string); isStr && strings.ContainsRune(sv, 0) {
					return nil, invalidf("field %q: default must not contain NUL bytes", f.Name)
				}
				defVal = cv
				if f.Required {
					defSQL, err = pgLiteral(f, cv)
					if err != nil {
						return nil, err
					}
				}
			}
			if f.Required || ch.Default != nil {
				rowCount, err := s.countTableRows(ctx, tx, table)
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
			physical, err := namer.allocate(f.Name)
			if err != nil {
				return nil, err
			}
			cur.Fields = append(cur.Fields, f)
			onDisk[f.Name] = false
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

			ddl := "ALTER TABLE " + table + " ADD COLUMN " + ident(physical) + " " + columnType(f)
			if f.Required {
				ddl += " NOT NULL"
			}
			if defSQL != "" {
				ddl += " DEFAULT " + defSQL
			}
			w.steps = append(w.steps, migrationStep{sql: ddl})
			if ch.Default != nil && !f.Required {
				w.steps = append(w.steps, migrationStep{
					sql:  "UPDATE " + table + " SET " + ident(physical) + " = $1",
					args: []any{pgWriteValue(f, defVal)},
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
			from, to, err := namer.rename(oldName, ch.To)
			if err != nil {
				return nil, err
			}
			if ch.To != ch.From {
				onDisk[ch.To] = onDisk[oldName]
				delete(onDisk, oldName)
				if src, ok := source[oldName]; ok {
					source[ch.To] = src
					delete(source, oldName)
				}
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
			if from != to {
				w.steps = append(w.steps, migrationStep{sql: "ALTER TABLE " + table + " RENAME COLUMN " + ident(from) + " TO " + ident(to)})
			}
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
			}
			idx := -1
			for i := range cur.Fields {
				if cur.Fields[i].Name == ch.Name {
					idx = i
					break
				}
			}
			cur.Fields = append(cur.Fields[:idx], cur.Fields[idx+1:]...)
			physical := namer.drop(ch.Name)
			delete(onDisk, ch.Name)
			delete(defaults, ch.Name)
			delete(source, ch.Name)
			plan.Operations = append(plan.Operations, fmt.Sprintf("drop_field %s", ch.Name))
			plan.Destructive = append(plan.Destructive, fmt.Sprintf("drop_field %s (the column and its data are removed permanently)", ch.Name))
			w.steps = append(w.steps, migrationStep{sql: "ALTER TABLE " + table + " DROP COLUMN " + ident(physical)})
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
			if len(vals) > 0 && onDisk[f.Name] {
				phys := source[f.Name]
				rows, err := tx.Query(ctx, "SELECT "+ident(phys)+", count(*) FROM "+table+" WHERE "+ident(phys)+" IS NOT NULL GROUP BY "+ident(phys))
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
			if f.Default != nil {
				if s, ok := f.Default.(string); ok && !schema.EnumAllows(vals, s) {
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
		case schema.OpSetRowAccess:
			if ch.Value == nil {
				return nil, invalidf("set_row_access requires value: true restricts rows to the principal who wrote them, false stops the filtering")
			}
			if *ch.Value {
				if cur.RowAccess == schema.RowAccessOwn {
					plan.Operations = append(plan.Operations, "set_row_access true (already enabled)")
					break
				}
				if err := store.ValidateOwnerCollision(cur.Fields); err != nil {
					return nil, err
				}
				var rows int64
				if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&rows); err != nil {
					return nil, err
				}
				if rows > 0 {
					return nil, invalidf("table %s already holds %d rows, so row_access cannot be enabled on it: no operation can write another principal's rows as that principal, so there is no honest way to assign owners to what is already there; create a new table with row_access and replay each owner's rows under their own identity, letting the server stamp them", old.Name, rows)
				}
				if !cur.HasOwner {
					w.steps = append(w.steps, migrationStep{sql: "ALTER TABLE " + table + " ADD COLUMN " + ident(schema.OwnerColumn) + ` text COLLATE "C"`})
				}
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
			return nil, invalidf("unknown migration op %q (valid: add_field, rename_field, drop_field, set_fulltext, set_vectorize, set_enum, set_row_access)", ch.Op)
		}
	}
	if len(plan.Destructive) > 0 && expectedVersion == 0 {
		return nil, invalidf("destructive changes require expected_version (from describe_table) so a stale plan cannot run against a schema that moved on: %s", strings.Join(plan.Destructive, "; "))
	}
	if err := schema.ValidateForMigration(cur.Fields, old.Fields); err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	if len(ftsFields(cur.Fields)) == 0 && len(ftsFields(old.Fields)) > 0 {
		rebuildFTSNeeded = true
	}

	plan.RebuildFulltext = rebuildFTSNeeded
	if rebuildFTSNeeded {
		var preds []string
		for _, f := range ftsFields(cur.Fields) {
			if !onDisk[f.Name] {
				if defaults[f.Name] != nil {
					preds = append(preds, "TRUE")
				}
				continue
			}
			preds = append(preds, ident(source[f.Name])+" IS NOT NULL")
		}
		if len(preds) > 0 {
			var count int64
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+strings.Join(preds, " OR ")).Scan(&count); err != nil {
				return nil, err
			}
			plan.FulltextReindexRows = count
		}
	}

	if vectorizeChanged {
		newVec := cur.VectorizeField()
		if newVec != nil {
			if emb.Embed == nil {
				return nil, invalidf("vectorize requires an embedding provider (set DOLMEN_EMBED_PROVIDER=local or openai)")
			}
			if emb.Identity == "" {
				return nil, invalidf("vectorize requires an embedding provider with a reported identity so backfilled rows are attributable to an embedding space; the active provider reports none — an operator must set DOLMEN_EMBED_PROVIDER (local or openai, plus credentials such as DOLMEN_EMBED_API_KEY for openai) and restart the server")
			}
			modelChanged := cur.EmbedSpace != "" && emb.Identity != "" && cur.EmbedSpace != emb.Identity
			plan.ClearsEmbeddings = old.VectorizeField() != nil || modelChanged
			w.embedding = true
			w.embed.clear = plan.ClearsEmbeddings
			w.embed.addColumn = old.VectorizeField() == nil
			w.embed.target = namer.columns[newVec.Name]
			if !onDisk[newVec.Name] {
				if text, ok := defaults[newVec.Name].(string); ok && text != "" {
					count, err := s.countTableRows(ctx, tx, table)
					if err != nil {
						return nil, err
					}
					plan.EmbedRows = count
					w.embed.constant = text
				}
			} else {
				w.embed.hasSource = true
				w.embed.source = source[newVec.Name]
				var count int64
				phys := ident(w.embed.source)
				if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+phys+" IS NOT NULL AND "+phys+" != ''").Scan(&count); err != nil {
					return nil, err
				}
				plan.EmbedRows = count
			}
			cur.EmbedSpace = emb.Identity
			cur.EmbedDim = 0
		} else if old.VectorizeField() != nil {
			plan.ClearsEmbeddings = true
			w.embed.clear = true
		}
	}

	w.rebuildFTS = rebuildFTSNeeded
	w.columns = namer.snapshot()
	cur.Version = old.Version + 1
	return w, nil
}

func ftsFields(fields []schema.Field) []schema.Field {
	out := []schema.Field{}
	for _, f := range fields {
		if f.Fulltext {
			out = append(out, f)
		}
	}
	return out
}

func describeValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func (s *Store) PlanMigration(ctx context.Context, ns, table string, changes []schema.Change, emb store.Embedder, expected store.Incarnation, scope *store.RowScope, scopeIncarnation store.Incarnation) (*store.MigrationPlan, error) {
	if scope != nil {
		return nil, store.ErrScopedPlanUnsupported
	}
	if len(changes) == 0 {
		return nil, invalidf("no changes given")
	}
	if expected.Version < 0 {
		return nil, invalidf("expected_version must be a positive schema version, got %d", expected.Version)
	}
	var plan *store.MigrationPlan
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := s.guardScope(ctx, tx, n, table, state, scopeIncarnation); err != nil {
			return err
		}
		if err := checkIncarnation(ns, state.incarnation, expected); err != nil {
			return err
		}
		w, err := s.planMigration(ctx, tx, n, state, changes, emb, int(expected.Version))
		if err != nil {
			return err
		}
		plan = w.plan
		plan.DryRun = true
		plan.Expected = state.incarnation
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func (s *Store) Migrate(ctx context.Context, ns, table string, changes []schema.Change, emb store.Embedder, expected store.Incarnation) (*schema.TableSchema, error) {
	if len(changes) == 0 {
		return nil, invalidf("no changes given")
	}
	if expected.Version < 0 {
		return nil, invalidf("expected_version must be a positive schema version, got %d", expected.Version)
	}
	for attempt := 0; attempt < 3; attempt++ {
		var state tableState
		var planned *migrationWork
		err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
			var err error
			state, err = s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			if err := checkIncarnation(ns, state.incarnation, expected); err != nil {
				return err
			}
			planned, err = s.planMigration(ctx, tx, n, state, changes, emb, int(expected.Version))
			return err
		})
		if err != nil {
			return nil, err
		}
		digests := map[int64][32]byte{}
		vectors := map[int64][]float32{}
		var constant []float32
		if planned.embedding {
			if planned.embed.constant != "" {
				vecs, err := store.EmbedTexts(ctx, planned.cur, table, []string{planned.embed.constant}, emb)
				if err != nil {
					return nil, err
				}
				constant = vecs[0]
			} else if planned.embed.hasSource {
				if err := s.backfillEmbeddings(ctx, ns, table, state, planned.embed.source, planned.cur, emb, digests, vectors); err != nil {
					return nil, err
				}
			}
		}
		var result *schema.TableSchema
		err = s.write(ctx, ns, state.incarnation.NsGen, func(tx pgx.Tx, n namespace) error {
			current, err := s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			if err := checkIncarnation(ns, current.incarnation, state.incarnation); err != nil {
				return err
			}
			work, err := s.planMigration(ctx, tx, n, current, changes, emb, int(expected.Version))
			if err != nil {
				return err
			}
			result = work.cur
			physical := ident(n.physical, current.physical)
			if work.rebuildFTS {
				if _, err := tx.Exec(ctx, "ALTER TABLE "+physical+" DROP COLUMN IF EXISTS "+ident(ftsColumn)); err != nil {
					return err
				}
			}
			for _, step := range work.steps {
				if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
					return err
				}
			}
			if work.rebuildFTS {
				if ddl := ftsColumnDDL(work.cur.Fields, work.columns); ddl != "" {
					if _, err := tx.Exec(ctx, "ALTER TABLE "+physical+" ADD COLUMN "+ddl); err != nil {
						return err
					}
					indexDDL, err := ftsIndexDDL(ctx, tx, n, current.physical)
					if err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, indexDDL); err != nil {
						return err
					}
				}
			}
			if work.embed.addColumn {
				if _, err := tx.Exec(ctx, "ALTER TABLE "+physical+` ADD COLUMN IF NOT EXISTS "_embedding" bytea`); err != nil {
					return err
				}
			}
			if work.embed.clear {
				if _, err := tx.Exec(ctx, "UPDATE "+physical+` SET "_embedding" = NULL`); err != nil {
					return err
				}
			}
			if work.embedding {
				left, err := s.applyEmbeddings(ctx, tx, physical, work, digests, vectors, constant)
				if err != nil {
					return err
				}
				if left {
					return errMigrationRetry
				}
			}
			if err := s.regrantQueryTable(ctx, tx, n, current.physical, work.cur.Fields, work.columns, work.cur.HasOwner); err != nil {
				return err
			}
			return s.saveMigration(ctx, tx, n, current, work, changes)
		})
		if errors.Is(err, errMigrationRetry) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, derr.New(derr.Conflict, "migration of %s.%s kept losing a race with concurrent writes while backfilling embeddings; retry the migration", ns, table)
}

const embedBackfillPage = 128

func (s *Store) backfillEmbeddings(ctx context.Context, ns, table string, state tableState, column string, sc *schema.TableSchema, emb store.Embedder, digests map[int64][32]byte, vectors map[int64][]float32) error {
	var after int64
	for {
		ids := []int64{}
		texts := []string{}
		err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
			physical := ident(n.physical, state.physical)
			col := ident(column)
			rows, err := tx.Query(ctx, "SELECT id,"+col+" FROM "+physical+" WHERE "+col+" IS NOT NULL AND "+col+" != '' AND id > $1 ORDER BY id LIMIT $2", after, embedBackfillPage)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				var text string
				if err := rows.Scan(&id, &text); err != nil {
					return err
				}
				ids = append(ids, id)
				texts = append(texts, text)
			}
			return rows.Err()
		})
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		vecs, err := store.EmbedTexts(ctx, sc, table, texts, emb)
		if err != nil {
			return err
		}
		for i, id := range ids {
			digests[id] = sha256.Sum256([]byte(texts[i]))
			vectors[id] = vecs[i]
		}
		after = ids[len(ids)-1]
	}
}

func (s *Store) applyEmbeddings(ctx context.Context, tx pgx.Tx, physical string, work *migrationWork, digests map[int64][32]byte, vectors map[int64][]float32, constant []float32) (bool, error) {
	col := ident(work.embed.target)
	if work.embed.constant != "" {
		if constant == nil {
			return false, nil
		}
		tag, err := tx.Exec(ctx, "UPDATE "+physical+` SET "_embedding" = $1 WHERE `+col+" IS NOT NULL AND "+col+" != ''", schema.EncodeVector(constant))
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() > 0 && work.cur.EmbedDim == 0 {
			work.cur.EmbedDim = len(constant)
		}
		return false, nil
	}
	rows, err := tx.Query(ctx, "SELECT id,"+col+" FROM "+physical+" WHERE "+col+" IS NOT NULL AND "+col+` != '' AND "_embedding" IS NULL ORDER BY id`)
	if err != nil {
		return false, err
	}
	type pending struct {
		id  int64
		vec []float32
	}
	var apply []pending
	stale := false
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return false, err
		}
		if digests[id] != sha256.Sum256([]byte(text)) {
			stale = true
			continue
		}
		vec, ok := vectors[id]
		if !ok {
			stale = true
			continue
		}
		apply = append(apply, pending{id: id, vec: vec})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if stale {
		return true, nil
	}
	for _, p := range apply {
		if work.cur.EmbedDim == 0 {
			work.cur.EmbedDim = len(p.vec)
		}
		if _, err := tx.Exec(ctx, "UPDATE "+physical+` SET "_embedding" = $1 WHERE id = $2`, schema.EncodeVector(p.vec), p.id); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (s *Store) regrantQueryTable(ctx context.Context, tx pgx.Tx, n namespace, physical string, fields []schema.Field, columns map[string]string, hasOwner bool) error {
	if s.queryRole == "" {
		return nil
	}
	return s.grantQueryTable(ctx, tx, n, physical, fields, columns, hasOwner)
}

func (s *Store) saveMigration(ctx context.Context, tx pgx.Tx, n namespace, current tableState, work *migrationWork, changes []schema.Change) error {
	raw, err := json.Marshal(work.cur)
	if err != nil {
		return err
	}
	columnJSON, err := json.Marshal(work.columns)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE "+s.relation("tables")+" SET schema_json=$1, columns_json=$2 WHERE namespace=$3 AND name=$4",
		string(raw), string(columnJSON), n.name, current.incarnation.Table); err != nil {
		return err
	}
	cj, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO "+s.relation("migrations")+
		"(namespace,table_name,drop_generation,id,from_version,to_version,changes_json) "+
		"SELECT $1,$2,$3,COALESCE(MAX(id),0)+1,$4,$5,$6 FROM "+s.relation("migrations")+
		" WHERE namespace=$1 AND table_name=$2 AND drop_generation=$3",
		n.name, current.incarnation.Table, current.incarnation.DropGen, work.plan.FromVersion, work.cur.Version, string(cj))
	return err
}

func (s *Store) ListMigrations(ctx context.Context, ns, table string, inc store.Incarnation) ([]store.Migration, error) {
	out := []store.Migration{}
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := checkIncarnation(ns, state.incarnation, inc); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT id,from_version,to_version,changes_json,at FROM "+s.relation("migrations")+
			" WHERE namespace=$1 AND table_name=$2 AND drop_generation=$3 ORDER BY id DESC", n.name, table, state.incarnation.DropGen)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m store.Migration
			var cj string
			if err := rows.Scan(&m.ID, &m.FromVersion, &m.ToVersion, &cj, &m.At); err != nil {
				return err
			}
			dec := json.NewDecoder(strings.NewReader(cj))
			dec.UseNumber()
			if err := dec.Decode(&m.Changes); err != nil {
				return fmt.Errorf("corrupt migration record %d for %s.%s: %w", m.ID, ns, table, err)
			}
			for j := range m.Changes {
				if m.Changes[j].Op != schema.OpSetFulltext && m.Changes[j].Op != schema.OpSetVectorize {
					m.Changes[j].Value = nil
				}
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}
