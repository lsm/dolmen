package store

import (
	"context"

	"github.com/lsm/dolmen/internal/schema"
)

type legacyStore struct{ *Store }

func legacy(s *Store) legacyStore { return legacyStore{s} }

func (l legacyStore) ListNamespaces() ([]string, error) {
	return l.Store.ListNamespaces(context.Background(), "", nil)
}

func (l legacyStore) CreateNamespace(nsName string) error {
	return l.Store.CreateNamespace(context.Background(), nsName, [16]byte{})
}

func (l legacyStore) DropNamespace(nsName string) error {
	return l.Store.DropNamespace(context.Background(), nsName, [16]byte{})
}

func (l legacyStore) ListTables(ctx context.Context, nsName string) ([]string, error) {
	return l.Store.ListTables(ctx, nsName, nil)
}

func (l legacyStore) CreateTable(ctx context.Context, nsName, table string, fields []schema.Field) (*schema.TableSchema, error) {
	return l.Store.CreateTable(ctx, nsName, table, fields, TableOpts{}, [16]byte{})
}

func (l legacyStore) DescribeTable(ctx context.Context, nsName, table string) (*schema.TableSchema, int64, error) {
	return l.Store.DescribeTable(ctx, nsName, table, nil, Incarnation{})
}

func (l legacyStore) DropTable(ctx context.Context, nsName, table string) error {
	return l.Store.DropTable(ctx, nsName, table, Incarnation{})
}

func (l legacyStore) PlanMigration(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expectedVersion int) (*MigrationPlan, error) {
	return l.Store.PlanMigration(ctx, nsName, table, changes, emb, Incarnation{Version: int64(expectedVersion)}, nil, Incarnation{})
}

func (l legacyStore) Migrate(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expectedVersion int) (*schema.TableSchema, error) {
	return l.Store.Migrate(ctx, nsName, table, changes, emb, Incarnation{Version: int64(expectedVersion)})
}

func (l legacyStore) ListMigrations(ctx context.Context, nsName, table string) ([]Migration, error) {
	return l.Store.ListMigrations(ctx, nsName, table, Incarnation{})
}

func (l legacyStore) Insert(ctx context.Context, nsName, table string, records []map[string]any, emb Embedder) ([]int64, error) {
	res, err := l.Store.Insert(ctx, nsName, table, records, WriteOpts{}, emb, nil, Incarnation{})
	return res.Ids, err
}

func (l legacyStore) InsertIdempotent(ctx context.Context, nsName, table string, records []map[string]any, emb Embedder, key string) (ids []int64, replayed bool, err error) {
	if key == "" {
		return nil, false, invalidf("idempotency key must not be empty")
	}
	res, err := l.Store.Insert(ctx, nsName, table, records, WriteOpts{IdempotencyKey: key}, emb, nil, Incarnation{})
	if err != nil {
		return nil, false, err
	}
	return res.Ids, res.Replayed, nil
}

func (l legacyStore) UpsertByKey(ctx context.Context, nsName, table string, keyFields []string, records []map[string]any, emb Embedder) (ids []int64, inserted, updated int, err error) {
	res, err := l.Store.UpsertByKey(ctx, nsName, table, keyFields, records, WriteOpts{}, emb, nil, Incarnation{})
	if err != nil {
		return nil, 0, 0, err
	}
	return res.Ids, int(res.Inserted), int(res.Updated), nil
}

func (l legacyStore) Upsert(ctx context.Context, nsName, table, where string, args []any, set map[string]any, emb Embedder) (UpsertResult, error) {
	res, err := l.Store.Upsert(ctx, nsName, table, where, args, set, WriteOpts{}, emb, nil, Incarnation{})
	if err != nil {
		return UpsertResult{}, err
	}
	return UpsertResult{Ids: res.Ids, Inserted: res.Inserted, Updated: res.Updated, Changes: res.Changes}, nil
}

func (l legacyStore) Update(ctx context.Context, nsName, table, where string, args []any, set map[string]any, emb Embedder) (int64, error) {
	res, err := l.Store.Update(ctx, nsName, table, where, args, set, emb, nil, Incarnation{})
	return res.Updated, err
}

func (l legacyStore) Delete(ctx context.Context, nsName, table, where string, args []any, opts DeleteOptions) (DeleteResult, error) {
	return l.Store.Delete(ctx, nsName, table, where, args, opts, nil, Incarnation{})
}

func (l legacyStore) Query(ctx context.Context, nsName, query string, args []any, offset, limit int) ([]map[string]any, bool, error) {
	res, err := l.Store.Query(ctx, nsName, query, args, [16]byte{}, Page{Offset: offset, Limit: limit})
	if err != nil {
		return nil, false, err
	}
	return res.Rows, res.Truncated, nil
}

func (l legacyStore) SearchFulltext(ctx context.Context, nsName, table, query string, offset, limit int, includeHidden bool, filter string, args []any) ([]map[string]any, bool, error) {
	res, err := l.Store.SearchFulltext(ctx, nsName, table, query, filter, args, includeHidden, nil, Incarnation{}, Page{Offset: offset, Limit: limit})
	if err != nil {
		return nil, false, err
	}
	return res.Rows, res.Truncated, nil
}

func (l legacyStore) SearchVector(ctx context.Context, nsName, table, column string, vec []float32, embedModel string, offset, limit int, includeHidden bool, filter string, args []any, minScore *float64) (VectorSearchResult, error) {
	res, err := l.Store.SearchVector(ctx, nsName, table, VectorQuery{
		Column:     column,
		Vec:        vec,
		EmbedModel: embedModel,
		Filter:     filter,
		Args:       args,
		MinScore:   minScore,
	}, includeHidden, nil, Incarnation{}, Page{Offset: offset, Limit: limit})
	if err != nil {
		return VectorSearchResult{}, err
	}
	return VectorSearchResult{Rows: res.Rows, Truncated: res.Truncated, Skipped: res.SkippedVectors}, nil
}

func (l legacyStore) ValidateVectorSearch(ctx context.Context, nsName, table, column string, textQuery bool, embedIdentity string) error {
	sc, _, err := l.Store.TableState(ctx, nsName, table, nil)
	if err != nil {
		return err
	}
	_, _, err = resolveVectorColumn(sc, table, column, textQuery, embedIdentity)
	return err
}
