package store

import (
	"context"
	"slices"
)

const MaxReadRowsIDs = 1000

func (s *Store) GetRows(ctx context.Context, nsName, table string, ids []int64, scope *RowScope, scopeIncarnation Incarnation) (QueryResult, error) {
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
	rows, complete, err := fetchByIDsScoped(ctx, tx, table, ids, projectionFromSchema(sc, false), scope)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Rows: rows, Truncated: !complete}, nil
}
