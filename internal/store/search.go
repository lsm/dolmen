package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	DefaultSearchLimit = 10
	MaxSearchLimit     = 200
)

func searchLimit(n int) int {
	if n <= 0 {
		return DefaultSearchLimit
	}
	if n > MaxSearchLimit {
		return MaxSearchLimit
	}
	return n
}

func (s *Store) SearchFulltext(ctx context.Context, nsName, table, query string, offset, limit int, includeHidden bool) ([]map[string]any, bool, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, false, err
	}
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, false, err
	}
	if len(sc.FTSFields()) == 0 {
		return nil, false, invalidf("table %s has no fulltext fields", table)
	}
	limit = searchLimit(limit)
	if offset < 0 {
		return nil, false, invalidf("offset must be non-negative")
	}

	// Fetch limit+1 ids so we can tell the caller whether more results exist.
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT rowid FROM %s WHERE %s MATCH ? ORDER BY rank, rowid LIMIT ? OFFSET ?`,
			q(ftsTable(table)), ftsTable(table)),
		query, limit+1, offset)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	out, complete, err := fetchByIDs(ctx, tx, table, ids, projectionFromSchema(sc, includeHidden))
	if err != nil {
		return nil, false, err
	}
	hasMore := !complete || len(out) > limit
	if len(out) > limit {
		out = out[:limit]
	}
	return out, hasMore, nil
}

type dbQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// fetchByIDs reads the full rows for ids (in that order) through proj, the
// shared typed-read projection. It returns complete=true when every row fit
// within the response budget, or false when a row was skipped because it would
// have exceeded the budget.
func fetchByIDs(ctx context.Context, db dbQueryer, table string, ids []int64, proj *projection) ([]map[string]any, bool, error) {
	if len(ids) == 0 {
		return []map[string]any{}, true, nil
	}
	values := make([]string, len(ids))
	args := make([]any, 0, len(ids)*2)
	for i, id := range ids {
		values[i] = "(?, ?)"
		args = append(args, i, id)
	}
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`WITH _ranked(pos, id) AS (VALUES %s) SELECT t.* FROM _ranked JOIN %s t ON t.id = _ranked.id ORDER BY _ranked.pos`,
			strings.Join(values, ", "), q(table)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, false, err
	}
	byID := map[int64]map[string]any{}
	labelBytes := 0
	for _, c := range cols {
		if proj.isHidden(c) {
			continue
		}
		labelBytes += encodedSize(c) + 16
	}
	total := 0
	complete := true
scan:
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, false, err
		}
		m := make(map[string]any, len(cols))
		var id int64
		rowBytes := 0
		for i, c := range cols {
			if proj.isHidden(c) {
				continue
			}
			if err := checkRowValue(c, vals[i]); err != nil {
				return nil, false, err
			}
			if c == "id" {
				if v, ok := vals[i].(int64); ok {
					id = v
				}
			}
			if total+rowBytes+rawValSize(vals[i]) > MaxQueryBytes {
				if len(byID) == 0 {
					return nil, false, invalidf("search result exceeds the %d MiB response budget on its first row", MaxQueryBytes>>20)
				}
				complete = false
				break scan
			}
			v := proj.decodeColumn(c, vals[i])
			m[c] = v
			rowBytes += proj.presentedSize(c, vals[i], v)
			if total+rowBytes+labelBytes > MaxQueryBytes {
				if len(byID) == 0 {
					return nil, false, invalidf("search result exceeds the %d MiB response budget on its first row", MaxQueryBytes>>20)
				}
				complete = false
				break scan
			}
		}
		total += rowBytes + labelBytes
		byID[id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, complete, nil
}

func (s *Store) Delete(ctx context.Context, nsName, table, where string, args []any) (int64, error) {
	where = strings.TrimSpace(where)
	if where == "" {
		return 0, invalidf("filter is required (pass \"1=1\" to delete everything)")
	}
	if hasStatementSeparator(where) {
		return 0, invalidf("multiple statements are not allowed in filter")
	}
	for i, a := range args {
		args[i] = normalizeArg(a)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return 0, err
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp._dolmen_delete_ids`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`CREATE TEMP TABLE _dolmen_delete_ids AS SELECT id FROM %s WHERE %s`, q(table), where), args...); err != nil {
		return 0, fmt.Errorf("%w: filter error: %w", ErrInvalid, err)
	}
	if len(sc.FTSFields()) > 0 {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE rowid IN (SELECT id FROM _dolmen_delete_ids)`, q(ftsTable(table)))); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM _dolmen_delete_ids)`, q(table)))
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE _dolmen_delete_ids`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}
