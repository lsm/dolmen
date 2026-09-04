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

func (s *Store) SearchFulltext(ctx context.Context, nsName, table, query string, offset, limit int, includeHidden bool, filter string, args []any) ([]map[string]any, bool, error) {
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
	filter = strings.TrimSpace(filter)
	if filter != "" {
		if strings.Contains(filter, ";") {
			return nil, false, invalidf("multiple statements are not allowed in filter")
		}
		if len(args) > 100 {
			return nil, false, invalidf("too many filter arguments")
		}
		for i, a := range args {
			args[i] = normalizeArg(a)
		}
	}

	// Fetch limit+1 ids so we can tell the caller whether more results exist.
	stmt := fmt.Sprintf(`SELECT rowid FROM %s WHERE %s MATCH ? ORDER BY rank, rowid LIMIT ? OFFSET ?`,
		q(ftsTable(table)), ftsTable(table))
	qargs := []any{query, limit + 1, offset}
	if filter != "" {
		// The filter restricts base-table rows before ranking, with the same
		// semantics as search_vector's filter: its WHERE expression runs
		// against the base table alone, so bare and table-qualified column
		// names resolve exactly as they do there — no join, so the FTS table's
		// duplicate column names (or a base field named rank) cannot make a
		// reference ambiguous.
		stmt = fmt.Sprintf(`SELECT rowid FROM %s WHERE %s MATCH ? AND rowid IN (SELECT id FROM %s WHERE %s) ORDER BY rank, rowid LIMIT ? OFFSET ?`,
			q(ftsTable(table)), ftsTable(table), q(table), filter)
		qargs = append(append([]any{query}, args...), limit+1, offset)
	}
	rows, err := tx.QueryContext(ctx, stmt, qargs...)
	if err != nil {
		if filter != "" {
			return nil, false, NewFilterError(filter, err)
		}
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
	// The (limit+1)th id is only a look-ahead for truncated — never fetch it,
	// or an invalid value in that row would fail the whole page instead of
	// returning the valid rows with truncated=true.
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	out, complete, err := fetchByIDs(ctx, tx, table, ids, projectionFromSchema(sc, includeHidden))
	if err != nil {
		return nil, false, err
	}
	return out, hasMore || !complete, nil
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

// DefaultDeleteLimit is the maximum number of matching rows a delete can
// remove without an explicit limit or confirm: true.
const DefaultDeleteLimit = 1000

// DeleteOptions controls how Delete behaves: dry-run preview, a user-supplied
// limit (threshold), and an explicit confirmation to delete beyond the limit.
type DeleteOptions struct {
	DryRun  bool
	Limit   int
	Confirm bool
}

// DeleteResult reports how many rows matched the filter and how many were
// actually deleted (zero when DryRun is true).
type DeleteResult struct {
	Matched int64
	Deleted int64
}

func (s *Store) Delete(ctx context.Context, nsName, table, where string, args []any, opts DeleteOptions) (DeleteResult, error) {
	where = strings.TrimSpace(where)
	if where == "" {
		return DeleteResult{}, invalidf("filter is required (pass \"1=1\" to delete everything)")
	}
	if hasStatementSeparator(where) {
		return DeleteResult{}, invalidf("multiple statements are not allowed in filter")
	}
	for i, a := range args {
		args[i] = normalizeArg(a)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return DeleteResult{}, err
	}

	// Dry-run is a pure count: use the read-only connection, validate the
	// table and filter, and return the matched rows without modifying data.
	if opts.DryRun {
		if _, err := loadSchema(ctx, n.ro, nsName, table); err != nil {
			return DeleteResult{}, err
		}
		var matched int64
		if err := n.ro.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s`, q(table), where), args...).Scan(&matched); err != nil {
			return DeleteResult{}, NewFilterError(where, err)
		}
		return DeleteResult{Matched: matched, Deleted: 0}, nil
	}

	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return DeleteResult{}, err
	}
	defer tx.Rollback()

	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return DeleteResult{}, err
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp._dolmen_delete_ids`); err != nil {
		return DeleteResult{}, err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`CREATE TEMP TABLE _dolmen_delete_ids AS SELECT id FROM %s WHERE %s`, q(table), where), args...); err != nil {
		return DeleteResult{}, NewFilterError(where, err)
	}

	var matched int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM _dolmen_delete_ids`).Scan(&matched); err != nil {
		return DeleteResult{}, err
	}

	limit := int64(DefaultDeleteLimit)
	if opts.Limit > 0 {
		limit = int64(opts.Limit)
	}
	if matched > limit && !opts.Confirm {
		return DeleteResult{}, invalidf("filter matched %d rows, exceeding the delete limit of %d; pass confirm: true to proceed or dry_run: true to preview", matched, limit)
	}

	if len(sc.FTSFields()) > 0 {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE rowid IN (SELECT id FROM _dolmen_delete_ids)`, q(ftsTable(table)))); err != nil {
			return DeleteResult{}, err
		}
	}
	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM _dolmen_delete_ids)`, q(table)))
	if err != nil {
		return DeleteResult{}, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return DeleteResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE _dolmen_delete_ids`); err != nil {
		return DeleteResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeleteResult{}, err
	}
	return DeleteResult{Matched: matched, Deleted: deleted}, nil
}
