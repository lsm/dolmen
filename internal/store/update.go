package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

// UpdateResult reports what an upsert did: how many rows matched the filter
// and were updated, or — when nothing matched — the id of the inserted record.
type UpdateResult struct {
	Updated  int64
	Inserted bool
	ID       int64
}

// Update sets the given fields on every row matching the SQL WHERE expression,
// validating values against the table schema and keeping search indexes
// consistent: full-text rows are reindexed when an indexed field changes, and
// rows are re-embedded when a vectorized field changes.
func (s *Store) Update(ctx context.Context, nsName, table, where string, args []any, set map[string]any, emb Embedder) (int64, error) {
	res, err := s.updateOrUpsert(ctx, nsName, table, where, args, set, emb, false)
	if err != nil {
		return 0, err
	}
	return res.Updated, nil
}

// Upsert updates every row matching the SQL WHERE expression; when no row
// matches, set is inserted as a new record instead (and must then satisfy
// required fields).
func (s *Store) Upsert(ctx context.Context, nsName, table, where string, args []any, set map[string]any, emb Embedder) (UpdateResult, error) {
	return s.updateOrUpsert(ctx, nsName, table, where, args, set, emb, true)
}

func (s *Store) updateOrUpsert(ctx context.Context, nsName, table, where string, args []any, set map[string]any, emb Embedder, allowInsert bool) (UpdateResult, error) {
	where = strings.TrimSpace(where)
	if where == "" {
		return UpdateResult{}, invalidf("filter is required (pass \"1=1\" to update every row)")
	}
	if strings.Contains(where, ";") {
		return UpdateResult{}, invalidf("multiple statements are not allowed in filter")
	}
	if len(set) == 0 {
		return UpdateResult{}, invalidf("set is required (at least one field to update)")
	}
	for i, a := range args {
		args[i] = normalizeArg(a)
	}
	normalized := make(map[string]any, len(set))
	for k, v := range set {
		lk := strings.ToLower(k)
		if _, exists := normalized[lk]; exists {
			return UpdateResult{}, invalidf("fields %q and its case variant collapse to %q; use one spelling", k, lk)
		}
		normalized[lk] = v
	}
	set = normalized

	n, err := s.ns(nsName)
	if err != nil {
		return UpdateResult{}, err
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return UpdateResult{}, err
	}
	defer tx.Rollback()

	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return UpdateResult{}, err
	}
	for k := range set {
		if sc.Field(k) == nil {
			return UpdateResult{}, invalidf("unknown field %q on table %s (see describe_table)", k, table)
		}
	}

	// Coerce in schema order so the generated SET clause is deterministic.
	cols := make([]string, 0, len(set))
	vals := make([]any, 0, len(set))
	coerced := make(map[string]any, len(set))
	for _, f := range sc.Fields {
		v, present := set[f.Name]
		if !present {
			continue
		}
		if f.Required && v == nil {
			return UpdateResult{}, invalidf("field %q is required and cannot be set to null", f.Name)
		}
		cv, err := coerceValue(f, v)
		if err != nil {
			return UpdateResult{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		cols = append(cols, q(f.Name))
		vals = append(vals, cv)
		coerced[f.Name] = cv
	}

	ftsTouched := false
	for _, f := range sc.Fields {
		if f.Fulltext {
			if _, ok := set[f.Name]; ok {
				ftsTouched = true
				break
			}
		}
	}

	var vf *schema.Field
	if f := sc.VectorizeField(); f != nil {
		if _, ok := set[f.Name]; ok {
			vf = f
		}
	}

	// Materialize matching ids first so the filter is evaluated exactly once,
	// against the pre-update state (mirrors Delete).
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp._dolmen_update_ids`); err != nil {
		return UpdateResult{}, err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`CREATE TEMP TABLE _dolmen_update_ids AS SELECT id FROM %s WHERE %s`, q(table), where), args...); err != nil {
		return UpdateResult{}, fmt.Errorf("%w: filter error: %w", ErrInvalid, err)
	}
	var matched int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM _dolmen_update_ids`).Scan(&matched); err != nil {
		return UpdateResult{}, err
	}

	// An unmatched upsert inserts: validate the candidate record before
	// embedding it, so a record that can never be inserted (missing required
	// field) fails deterministically instead of paying for an embedding.
	if allowInsert && matched == 0 {
		for _, f := range sc.Fields {
			v, present := set[f.Name]
			if present && v != nil {
				continue
			}
			if f.Required {
				return UpdateResult{}, invalidf("field %q is required (no row matched the filter, so upsert would insert a new record)", f.Name)
			}
		}
	}

	// Embed only when there is work that needs the vector: matched rows to
	// re-embed, or an upsert insert about to run. A zero-match update skips
	// the embedding provider entirely.
	persistMeta := false
	var vec []float32
	if vf != nil && (matched > 0 || allowInsert) {
		if text, _ := coerced[vf.Name].(string); text != "" {
			persistMeta = sc.EmbedSpace == "" || sc.EmbedDim == 0
			vec, err = embedForUpdate(ctx, sc, table, text, emb)
			if err != nil {
				return UpdateResult{}, err
			}
		}
	}

	result := UpdateResult{}
	switch {
	case matched > 0:
		assignments := make([]string, len(cols))
		for i, c := range cols {
			assignments[i] = c + ` = ?`
		}
		avals := vals
		if vf != nil {
			if vec != nil {
				assignments = append(assignments, `"_embedding" = ?`)
				avals = append(avals, schema.EncodeVector(vec))
			} else {
				assignments = append(assignments, `"_embedding" = NULL`)
			}
		}
		res, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET %s WHERE id IN (SELECT id FROM _dolmen_update_ids)`,
				q(table), strings.Join(assignments, ", ")), avals...)
		if err != nil {
			return UpdateResult{}, fmt.Errorf("update %s: %w", table, err)
		}
		updated, err := res.RowsAffected()
		if err != nil {
			return UpdateResult{}, err
		}
		if updated > 0 && ftsTouched {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM %s WHERE rowid IN (SELECT id FROM _dolmen_update_ids)`, q(ftsTable(table)))); err != nil {
				return UpdateResult{}, err
			}
			if err := reindexFTSRows(ctx, tx, table, sc.FTSFields()); err != nil {
				return UpdateResult{}, fmt.Errorf("update search index for %s: %w", table, err)
			}
		}
		result.Updated = updated
	case allowInsert:
		icols := cols
		ivals := vals
		if vec != nil {
			icols = append(icols, `"_embedding"`)
			ivals = append(ivals, schema.EncodeVector(vec))
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(icols)), ",")
		res, err := tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, q(table), strings.Join(icols, ", "), ph), ivals...)
		if err != nil {
			return UpdateResult{}, fmt.Errorf("insert into %s: %w", table, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return UpdateResult{}, err
		}
		if fts := sc.FTSFields(); len(fts) > 0 {
			if err := insertFTSRow(ctx, tx, table, fts, id, coerced); err != nil {
				return UpdateResult{}, fmt.Errorf("update search index for %s: %w", table, err)
			}
		}
		result.Inserted = true
		result.ID = id
	}

	if persistMeta && (result.Updated > 0 || result.Inserted) {
		if sc.EmbedSpace == "" {
			sc.EmbedSpace = emb.Identity
		}
		raw, err := json.Marshal(sc)
		if err != nil {
			return UpdateResult{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE _dolmen_tables SET schema_json = ? WHERE name = ?`, string(raw), table); err != nil {
			return UpdateResult{}, err
		}
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE _dolmen_update_ids`); err != nil {
		return UpdateResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return UpdateResult{}, err
	}
	return result, nil
}

func embedForUpdate(ctx context.Context, sc *schema.TableSchema, table, text string, emb Embedder) ([]float32, error) {
	if emb.Embed == nil {
		return nil, invalidf("table %s uses vectorize but no embedding provider is configured", table)
	}
	if emb.Identity == "" {
		return nil, invalidf("table %s uses vectorize but the active embedding provider reports no identity; configure the provider before updating so rows are attributable to an embedding space", table)
	}
	if sc.EmbedSpace != "" && sc.EmbedSpace != emb.Identity {
		return nil, invalidf("embedding provider changed: table rows were embedded by %q but the active provider is %q; re-embed via migrate (set_vectorize off, then on)", sc.EmbedSpace, emb.Identity)
	}
	vecs, err := emb.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("embedding failed: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embedding provider returned %d vectors for 1 text", len(vecs))
	}
	v := vecs[0]
	if len(v) == 0 {
		return nil, invalidf("embedding provider returned a zero-dimensional vector for table %s", table)
	}
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, invalidf("embedding provider returned a non-finite vector component for table %s", table)
		}
	}
	if sc.EmbedDim == 0 {
		sc.EmbedDim = len(v)
	} else if len(v) != sc.EmbedDim {
		return nil, invalidf("embedding provider returned %d-dimensional vectors but table %s stores %d-dimensional embeddings; re-embed via migrate (set_vectorize off, then on) if the provider changed", len(v), table, sc.EmbedDim)
	}
	return v, nil
}

// reindexFTSRows rebuilds full-text rows for the ids in _dolmen_update_ids
// from the current base-table values.
func reindexFTSRows(ctx context.Context, tx *sql.Tx, table string, fts []schema.Field) error {
	cols := make([]string, len(fts))
	sel := make([]string, len(fts))
	where := make([]string, len(fts))
	for i, f := range fts {
		cols[i] = q(f.Name)
		sel[i] = q(f.Name)
		where[i] = fmt.Sprintf(`%s IS NOT NULL`, q(f.Name))
	}
	stmt := fmt.Sprintf(`INSERT INTO %s(rowid, %s) SELECT id, %s FROM %s WHERE id IN (SELECT id FROM _dolmen_update_ids) AND (%s)`,
		q(ftsTable(table)), strings.Join(cols, ", "), strings.Join(sel, ", "), q(table), strings.Join(where, " OR "))
	_, err := tx.ExecContext(ctx, stmt)
	return err
}

// insertFTSRow adds the full-text row for a freshly inserted record.
func insertFTSRow(ctx context.Context, tx *sql.Tx, table string, fts []schema.Field, id int64, rec map[string]any) error {
	fcols := make([]string, len(fts))
	fvals := make([]any, len(fts)+1)
	fvals[0] = id
	for j, f := range fts {
		fcols[j] = q(f.Name)
		fvals[j+1] = ftsText(rec[f.Name])
	}
	fph := strings.TrimSuffix(strings.Repeat("?,", len(fts)+1), ",")
	_, err := tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (rowid, %s) VALUES (%s)`, q(ftsTable(table)), strings.Join(fcols, ", "), fph), fvals...)
	return err
}
