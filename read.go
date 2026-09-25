package dolmen

import (
	"context"
	"regexp"
	"strings"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

const (
	maxArgsPerCall   = 100
	maxOffsetPerCall = 1000000000
)

var queryShapeRe = regexp.MustCompile(`^\s*(?i:select|with)\b`)

var tableNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func validTableName(t string) bool {
	return tableNameRe.MatchString(t) && !strings.Contains(t, "__fts") && !strings.HasPrefix(t, "sqlite_")
}

type QueryOptions struct {
	Args   []any
	Offset int
	Limit  int
}

type QueryResult struct {
	Rows      []map[string]any
	Truncated bool
}

func (s *Store) GetRows(ctx context.Context, namespace, table string, ids []int64) (QueryResult, error) {
	return s.RevealRows(ctx, namespace, table, ids, nil)
}

func (s *Store) RevealRows(ctx context.Context, namespace, table string, ids []int64, reveal []string) (r0 QueryResult, err error) {
	ctx, span := s.startOp(ctx, "read_rows", namespace, table)
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return QueryResult{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return QueryResult{}, facadeErr(err)
	}
	if len(ids) > store.MaxReadRowsIDs {
		return QueryResult{}, derr.New(derr.InvalidRequest, "GetRows accepts at most %d ids per call, got %d", store.MaxReadRowsIDs, len(ids))
	}
	if ids == nil {
		return QueryResult{}, derr.New(derr.InvalidRequest, "GetRows requires ids: pass the ids a write returned or a query projected; an empty non-nil slice selects nothing")
	}
	if tbl := ops.NormalizeTable(table); tbl == "" || !validTableName(tbl) {
		return QueryResult{}, derr.New(derr.InvalidRequest, "table must match ^[a-z][a-z0-9_]{0,63}$ and not contain __fts or start with sqlite_")
	}
	ns := ops.NormalizeNamespace(namespace)
	ownIds := append([]int64(nil), ids...)
	res, err := s.eng.GetRows(store.WithReveal(ctx, reveal), ns, ops.NormalizeTable(table), ownIds, nil, store.Incarnation{})
	if err != nil {
		return QueryResult{}, facadeErr(err)
	}
	return QueryResult{Rows: res.Rows, Truncated: res.Truncated}, nil
}

func (s *Store) Query(ctx context.Context, namespace, sql string, opts QueryOptions) (r0 QueryResult, err error) {
	ctx, span := s.startOp(ctx, "query", namespace, "")
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return QueryResult{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return QueryResult{}, facadeErr(err)
	}
	if opts.Limit != 0 && (opts.Limit < 1 || opts.Limit > store.MaxPageLimit) {
		return QueryResult{}, derr.New(derr.InvalidRequest, "QueryOptions.Limit must be between 1 and %d (0 keeps the default page size), got %d", store.MaxPageLimit, opts.Limit)
	}
	if strings.TrimSpace(sql) == "" || len([]rune(sql)) > store.MaxQueryRunes {
		return QueryResult{}, derr.New(derr.InvalidRequest, "sql must be a non-empty read-only statement of at most %d characters", store.MaxQueryRunes)
	}
	if !queryShapeRe.MatchString(sql) {
		return QueryResult{}, derr.New(derr.InvalidRequest, "sql must start with SELECT or WITH (read-only queries only)")
	}
	if len(opts.Args) > maxArgsPerCall {
		return QueryResult{}, derr.New(derr.InvalidRequest, "at most %d bind parameters are accepted per call, got %d", maxArgsPerCall, len(opts.Args))
	}
	if opts.Offset < 0 || opts.Offset > maxOffsetPerCall {
		return QueryResult{}, derr.New(derr.InvalidRequest, "QueryOptions.Offset must be between 0 and %d, got %d", maxOffsetPerCall, opts.Offset)
	}
	ns := ops.NormalizeNamespace(namespace)
	args := append([]any(nil), opts.Args...)
	res, err := s.eng.Query(ctx, ns, sql, args, [16]byte{}, store.Page{Offset: opts.Offset, Limit: opts.Limit})
	if err != nil {
		return QueryResult{}, facadeErr(err)
	}
	return QueryResult{Rows: res.Rows, Truncated: res.Truncated}, nil
}
