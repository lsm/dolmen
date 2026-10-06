package store

import (
	"context"
	"fmt"
	"slices"
)

const MaxReadRowsIDs = 1000

const DefaultReadRowsLimit = 100

func ValidateRowPage(afterID int64, limit int) error {
	if afterID < 0 {
		return invalidf("after_id must be 0 or a row id, got %d", afterID)
	}
	if limit < 1 || limit > MaxReadRowsIDs {
		return invalidf("limit must be between 1 and %d, got %d", MaxReadRowsIDs, limit)
	}
	return nil
}

func (s *Store) GetRows(ctx context.Context, nsName, table string, ids []int64, scope *RowScope, scopeIncarnation Incarnation) (_ QueryResult, err error) {
	ctx, span := s.tr.Op(ctx, "SELECT", nsName, table)
	defer func() { s.tr.End(ctx, span, err) }()
	if len(ids) > MaxReadRowsIDs {
		return QueryResult{}, invalidf("read_rows accepts at most %d ids per request, got %d", MaxReadRowsIDs, len(ids))
	}
	n, err := s.ns(nsName)
	if err != nil {
		return QueryResult{}, err
	}
	defer n.unpin()
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return QueryResult{}, err
	}
	defer tx.Rollback()
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return QueryResult{}, err
	}

	if err := checkScopeIncarnation(ctx, tx, nsName, table, scopeIncarnation); err != nil {
		return QueryResult{}, err
	}
	if err := scopeUsable(scope, sc); err != nil {
		return QueryResult{}, err
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	proj, err := s.readProjection(ctx, sc, false)
	if err != nil {
		return QueryResult{}, err
	}
	rows, complete, err := fetchByIDsScoped(ctx, tx, table, ids, proj, scope)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Rows: rows, Truncated: !complete}, nil
}

func (s *Store) ListRows(ctx context.Context, nsName, table string, afterID int64, limit int, scope *RowScope, scopeIncarnation Incarnation) (_ QueryResult, err error) {
	ctx, span := s.tr.Op(ctx, "SELECT", nsName, table)
	defer func() { s.tr.End(ctx, span, err) }()
	if err := ValidateRowPage(afterID, limit); err != nil {
		return QueryResult{}, err
	}
	n, err := s.ns(nsName)
	if err != nil {
		return QueryResult{}, err
	}
	defer n.unpin()
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return QueryResult{}, err
	}
	defer tx.Rollback()
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return QueryResult{}, err
	}
	if err := checkScopeIncarnation(ctx, tx, nsName, table, scopeIncarnation); err != nil {
		return QueryResult{}, err
	}
	if err := scopeUsable(scope, sc); err != nil {
		return QueryResult{}, err
	}
	stmt := fmt.Sprintf("SELECT id FROM %s WHERE id > ?", q(table))
	args := []any{afterID}
	if clause, sargs := scopeClause(scope, ""); clause != "" {
		stmt += " AND " + clause
		args = append(args, sargs...)
	}
	rows, err := tx.QueryContext(ctx, stmt+" ORDER BY id LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return QueryResult{}, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return QueryResult{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return QueryResult{}, err
	}
	if err := rows.Close(); err != nil {
		return QueryResult{}, err
	}
	more := len(ids) > limit
	if more {
		ids = ids[:limit]
	}
	proj, err := s.readProjection(ctx, sc, false)
	if err != nil {
		return QueryResult{}, err
	}
	got, complete, err := fetchByIDsScoped(ctx, tx, table, ids, proj, scope)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Rows: got, Truncated: more || !complete}, nil
}
