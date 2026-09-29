package dolmen

import (
	"context"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

type BatchWriteKind string

const (
	BatchInsert      BatchWriteKind = "insert"
	BatchUpdate      BatchWriteKind = "update"
	BatchDelete      BatchWriteKind = "delete"
	BatchUpsert      BatchWriteKind = "upsert"
	BatchUpsertByKey BatchWriteKind = "upsert_by_key"
)

type BatchWrite struct {
	Kind    BatchWriteKind
	Table   string
	Records []map[string]any
	On      []string
	Filter  string
	Args    []any
	Set     map[string]any
}

type BatchOptions struct {
	IdempotencyKey string
	Limit          int
	Confirm        bool
}

type BatchWriteResult struct {
	Kind     BatchWriteKind
	Ids      []int64
	Inserted int64
	Updated  int64
	Matched  int64
	Deleted  int64
	Changes  ChangeRange
}

type BatchResult struct {
	Replayed bool
	Results  []BatchWriteResult
	Changes  ChangeRange
}

func (s *Store) Batch(ctx context.Context, namespace string, writes []BatchWrite, opts BatchOptions) (r0 BatchResult, err error) {
	ctx, span := s.startOp(ctx, "batch", namespace, "")
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return BatchResult{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return BatchResult{}, facadeErr(ctx, err)
	}
	if len(writes) == 0 {
		return BatchResult{}, derr.New(derr.InvalidRequest, "a batch needs at least one write")
	}
	if opts.Limit < 0 {
		return BatchResult{}, derr.New(derr.InvalidRequest, "BatchOptions.Limit must not be negative (0 keeps the default confirm threshold)")
	}
	eng := make([]store.BatchWrite, 0, len(writes))
	for i, w := range writes {
		ew, err := batchEngineWrite(i, w)
		if err != nil {
			return BatchResult{}, err
		}
		eng = append(eng, ew)
	}
	ns := ops.NormalizeNamespace(namespace)
	if err := ops.EnsureNamespace(ctx, s.eng, ns); err != nil {
		return BatchResult{}, facadeErr(ctx, err)
	}
	res, err := s.eng.Batch(ctx, ns, eng, store.BatchOpts{
		IdempotencyKey: opts.IdempotencyKey,
		Limit:          opts.Limit,
		Confirm:        opts.Confirm,
	}, s.embedder(), nil, store.Incarnation{})
	if err != nil {
		return BatchResult{}, facadeErr(ctx, err)
	}
	out := BatchResult{
		Replayed: res.Replayed,
		Results:  make([]BatchWriteResult, 0, len(res.Results)),
		Changes:  ChangeRange(res.Changes),
	}
	for _, r := range res.Results {
		out.Results = append(out.Results, BatchWriteResult{
			Kind:     BatchWriteKind(r.Kind),
			Ids:      r.Ids,
			Inserted: r.Inserted,
			Updated:  r.Updated,
			Matched:  r.Matched,
			Deleted:  r.Deleted,
			Changes:  ChangeRange(r.Changes),
		})
	}
	return out, nil
}

func batchEngineWrite(i int, w BatchWrite) (store.BatchWrite, error) {
	ew := store.BatchWrite{
		Kind:    store.BatchWriteKind(w.Kind),
		Records: w.Records,
		On:      w.On,
		Filter:  w.Filter,
		Args:    append([]any(nil), w.Args...),
		Set:     w.Set,
	}
	switch w.Kind {
	case BatchInsert, BatchUpdate, BatchDelete, BatchUpsert, BatchUpsertByKey:
	default:
		return store.BatchWrite{}, derr.New(derr.InvalidRequest, "writes[%d]: unknown write kind %q; one of delete, insert, update, upsert, upsert_by_key", i, string(w.Kind))
	}
	ew.Table = ops.NormalizeTable(w.Table)
	if ew.Table == "" {
		return store.BatchWrite{}, derr.New(derr.InvalidRequest, "writes[%d]: table is required", i)
	}
	for j, rec := range ew.Records {
		if rec == nil {
			return store.BatchWrite{}, derr.New(derr.InvalidRequest, "writes[%d]: records[%d] must be an object, not null", i, j)
		}
	}
	return ew, nil
}
