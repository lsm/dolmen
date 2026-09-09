package store

import (
	"context"
	"slices"
)

// MaxReadRowsIDs is the per-request id cap on read_rows (§2): a request body
// under the 32 MiB envelope cap can still name millions of ids, which would
// blow past any engine's bind limits, so the cap is enforced at the seam.
const MaxReadRowsIDs = 1000

// GetRows is the id-addressed scoped fetch behind read_rows (§2, §6.2): the
// realtime recovery path (§9.3) and agents generally need by-id reads
// without raw SQL's namespace-wide gate (§4.4). Ids name a set: the response
// carries each found row once, in ascending id order, and ids that are
// missing — or outside the caller's visible set — are simply absent, never
// an error (authz-precedes-existence, §2). TODO(9d): scope and
// scopeIncarnation are ignored while auth is off — a non-nil scope will
// filter visible rows.
func (s *Store) GetRows(ctx context.Context, nsName, table string, ids []int64, scope *RowScope, scopeIncarnation Incarnation) (QueryResult, error) {
	if len(ids) > MaxReadRowsIDs {
		return QueryResult{}, invalidf("read_rows accepts at most %d ids per request, got %d", MaxReadRowsIDs, len(ids))
	}
	n, err := s.ns(nsName)
	if err != nil {
		return QueryResult{}, err
	}
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return QueryResult{}, err
	}
	defer tx.Rollback()
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return QueryResult{}, err
	}
	// Ascending id order with duplicates collapsed: ids address a set of
	// rows, so the response is the found rows sorted by id, each once.
	slices.Sort(ids)
	ids = slices.Compact(ids)
	rows, complete, err := fetchByIDs(ctx, tx, table, ids, projectionFromSchema(sc, false))
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Rows: rows, Truncated: !complete}, nil
}
