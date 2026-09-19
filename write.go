package dolmen

import (
	"context"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

type ChangeRange struct {
	First int64
	Last  int64
	Count int64
}

type InsertResult struct {
	Ids      []int64
	Inserted int64
	Updated  int64
	Replayed bool
	Changes  ChangeRange
}

type UpdateResult struct {
	Updated int64
	Changes ChangeRange
}

type DeleteResult struct {
	Matched int64
	Deleted int64
	Changes ChangeRange
}

type InsertOptions struct {
	IdempotencyKey string
}

type UpdateOptions struct {
	Filter string
	Args   []any
	Set    map[string]any
}

type DeleteOptions struct {
	Filter  string
	Args    []any
	DryRun  bool
	Limit   int
	Confirm bool
}

func (s *Store) embedder() store.Embedder {
	if s.emb == nil {
		return store.Embedder{}
	}
	return ops.Embedder(s.emb)
}

func (s *Store) Insert(ctx context.Context, namespace, table string, records []map[string]any, opts InsertOptions) (InsertResult, error) {
	if err := s.begin(); err != nil {
		return InsertResult{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return InsertResult{}, facadeErr(err)
	}
	for i, rec := range records {
		if rec == nil {
			return InsertResult{}, derr.New(derr.InvalidRequest, "records[%d] must be an object, not null", i)
		}
	}
	ns := ops.NormalizeNamespace(namespace)
	if err := ops.EnsureNamespace(ctx, s.eng, ns); err != nil {
		return InsertResult{}, facadeErr(err)
	}
	res, err := s.eng.Insert(ctx, ns, ops.NormalizeTable(table), records,
		store.WriteOpts{IdempotencyKey: opts.IdempotencyKey}, s.embedder(), nil, store.Incarnation{})
	if err != nil {
		return InsertResult{}, facadeErr(err)
	}
	inserted := int64(len(res.Ids))
	if res.Replayed {
		inserted = 0
	}
	return InsertResult{
		Ids:      res.Ids,
		Inserted: inserted,
		Replayed: res.Replayed,
		Changes:  ChangeRange(res.Changes),
	}, nil
}

func (s *Store) UpsertByKey(ctx context.Context, namespace, table string, on []string, records []map[string]any) (InsertResult, error) {
	if err := s.begin(); err != nil {
		return InsertResult{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return InsertResult{}, facadeErr(err)
	}
	ns := ops.NormalizeNamespace(namespace)
	if err := ops.EnsureNamespace(ctx, s.eng, ns); err != nil {
		return InsertResult{}, facadeErr(err)
	}
	res, err := s.eng.UpsertByKey(ctx, ns, ops.NormalizeTable(table), on, records,
		store.WriteOpts{}, s.embedder(), nil, store.Incarnation{})
	if err != nil {
		return InsertResult{}, facadeErr(err)
	}
	return InsertResult{
		Ids:      res.Ids,
		Inserted: res.Inserted,
		Updated:  res.Updated,
		Replayed: res.Replayed,
		Changes:  ChangeRange(res.Changes),
	}, nil
}

func (s *Store) Update(ctx context.Context, namespace, table string, opts UpdateOptions) (UpdateResult, error) {
	if err := s.begin(); err != nil {
		return UpdateResult{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return UpdateResult{}, facadeErr(err)
	}
	args := append([]any(nil), opts.Args...)
	ns := ops.NormalizeNamespace(namespace)
	if err := ops.EnsureNamespace(ctx, s.eng, ns); err != nil {
		return UpdateResult{}, facadeErr(err)
	}
	res, err := s.eng.Update(ctx, ns, ops.NormalizeTable(table), opts.Filter, args, opts.Set,
		s.embedder(), nil, store.Incarnation{})
	if err != nil {
		return UpdateResult{}, facadeErr(err)
	}
	return UpdateResult{Updated: res.Updated, Changes: ChangeRange(res.Changes)}, nil
}

func (s *Store) Delete(ctx context.Context, namespace, table string, opts DeleteOptions) (DeleteResult, error) {
	if err := s.begin(); err != nil {
		return DeleteResult{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return DeleteResult{}, facadeErr(err)
	}
	if opts.Limit < 0 {
		return DeleteResult{}, derr.New(derr.InvalidRequest, "DeleteOptions.Limit must not be negative (0 keeps the default confirm threshold)")
	}
	args := append([]any(nil), opts.Args...)
	ns := ops.NormalizeNamespace(namespace)
	if err := ops.EnsureNamespace(ctx, s.eng, ns); err != nil {
		return DeleteResult{}, facadeErr(err)
	}
	res, err := s.eng.Delete(ctx, ns, ops.NormalizeTable(table), opts.Filter, args, store.DeleteOptions{
		DryRun:  opts.DryRun,
		Limit:   opts.Limit,
		Confirm: opts.Confirm,
	}, nil, store.Incarnation{})
	if err != nil {
		return DeleteResult{}, facadeErr(err)
	}
	return DeleteResult{Matched: res.Matched, Deleted: res.Deleted, Changes: ChangeRange(res.Changes)}, nil
}
