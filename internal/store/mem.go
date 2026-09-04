package store

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/lsm/dolmen/internal/schema"
)

// MemEngine is a minimal in-memory Engine: the second implementation that
// proves the store.Engine seam is real (see engine.go; dolmen#76). It
// carries the namespace and table registry — the ops whose behavior is
// pure schema with no engine state — by delegating to the same schema-layer
// helpers the SQLite adapter uses, and rejects every op that would need row
// storage, SQL evaluation, or migration execution with a legible
// not-implemented error. It holds no files, is never constructed by the
// server, and exists so the conformance harness can run the op layer over a
// non-SQLite engine and #164 starts from a tested seam.
//
// Stored schemas are immutable once registered, so reads can hand out the
// shared pointers; only the maps are guarded, and every read works on a
// snapshot taken under the lock.
type MemEngine struct {
	mu  sync.Mutex
	nss map[string]map[string]*schema.TableSchema
}

// OpenMem returns an empty in-memory engine.
func OpenMem() *MemEngine {
	return &MemEngine{nss: map[string]map[string]*schema.TableSchema{}}
}

// MemEngine implements Engine.
var _ Engine = (*MemEngine)(nil)

// snapshot returns a copy of the namespace's name->schema map, creating the
// namespace when absent — namespaces come into existence on first use,
// mirroring the SQLite engine's implicit database files.
func (m *MemEngine) snapshot(nsName string) (map[string]*schema.TableSchema, error) {
	if err := validateNS(nsName); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ns, ok := m.nss[nsName]
	if !ok {
		ns = map[string]*schema.TableSchema{}
		m.nss[nsName] = ns
	}
	out := make(map[string]*schema.TableSchema, len(ns))
	for name, sc := range ns {
		out[name] = sc
	}
	return out, nil
}

// loadSchema reads one table's schema, mirroring the SQLite adapter's
// not-found error so the op layer classifies both engines alike.
func (m *MemEngine) loadSchema(nsName, table string) (*schema.TableSchema, error) {
	ns, err := m.snapshot(nsName)
	if err != nil {
		return nil, err
	}
	sc, ok := ns[table]
	if !ok {
		return nil, fmt.Errorf("%w: table %s.%s", ErrNotFound, nsName, table)
	}
	return sc, nil
}

func (m *MemEngine) ListNamespaces() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.nss))
	for name := range m.nss {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (m *MemEngine) CreateNamespace(nsName string) error {
	if err := validateNS(nsName); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nss[nsName]; ok {
		return invalidf("namespace %s already exists", nsName)
	}
	m.nss[nsName] = map[string]*schema.TableSchema{}
	return nil
}

func (m *MemEngine) DropNamespace(nsName string) error {
	if err := validateNS(nsName); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nss[nsName]; !ok {
		return fmt.Errorf("%w: namespace %s", ErrNotFound, nsName)
	}
	delete(m.nss, nsName)
	return nil
}

func (m *MemEngine) ListTables(ctx context.Context, nsName string) ([]string, error) {
	ns, err := m.snapshot(nsName)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ns))
	for name := range ns {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (m *MemEngine) DescribeTable(ctx context.Context, nsName, table string) (*schema.TableSchema, int64, error) {
	sc, err := m.loadSchema(nsName, table)
	if err != nil {
		return nil, 0, err
	}
	// The registry holds no rows, so the honest count is zero.
	return sc, 0, nil
}

func (m *MemEngine) CreateTable(ctx context.Context, nsName, table string, fields []schema.Field) (*schema.TableSchema, error) {
	if err := schema.ValidateTableName(table); err != nil {
		return nil, invalidf("%s", err)
	}
	if len(fields) > MaxFieldsPerTable {
		return nil, invalidf("too many fields: %d (max %d)", len(fields), MaxFieldsPerTable)
	}
	fields = schema.Normalize(fields)
	if err := schema.Validate(fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := validateFieldDefaults(fields); err != nil {
		return nil, err
	}
	if err := validateNS(nsName); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ns, ok := m.nss[nsName]
	if !ok {
		ns = map[string]*schema.TableSchema{}
		m.nss[nsName] = ns
	}
	if _, ok := ns[table]; ok {
		return nil, invalidf("table %s.%s already exists", nsName, table)
	}
	sc := &schema.TableSchema{Namespace: nsName, Name: table, Version: 1, Fields: fields}
	ns[table] = sc
	return sc, nil
}

func (m *MemEngine) DropTable(ctx context.Context, nsName, table string) error {
	if _, err := m.loadSchema(nsName, table); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.nss[nsName], table)
	return nil
}

func (m *MemEngine) ListMigrations(ctx context.Context, nsName, table string) ([]Migration, error) {
	if _, err := m.loadSchema(nsName, table); err != nil {
		return nil, err
	}
	// Creating the table is version 1 and predates the log, and this engine
	// cannot migrate — so every table's history is empty.
	return []Migration{}, nil
}

func (m *MemEngine) ValidateVectorSearch(ctx context.Context, nsName, table, column string, textQuery bool, embedIdentity string) error {
	sc, err := m.loadSchema(nsName, table)
	if err != nil {
		return err
	}
	_, _, err = resolveVectorColumn(sc, table, column, textQuery, embedIdentity)
	return err
}

// errUnimplemented rejects the ops MemEngine does not carry: they need row
// storage, SQL evaluation, or migration execution. It wraps ErrInvalid so
// the refusal surfaces through the API as a legible invalid_request naming
// the seam, not as an opaque internal_error.
func errUnimplemented(op string) error {
	return invalidf("%s is not implemented by this engine (MemEngine exists to prove the store.Engine seam; see dolmen#76)", op)
}

func (m *MemEngine) Migrate(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expectedVersion int) (*schema.TableSchema, error) {
	return nil, errUnimplemented("migrate")
}

func (m *MemEngine) PlanMigration(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expectedVersion int) (*MigrationPlan, error) {
	return nil, errUnimplemented("migrate dry-run")
}

func (m *MemEngine) Insert(ctx context.Context, nsName, table string, records []map[string]any, emb Embedder) ([]int64, error) {
	return nil, errUnimplemented("insert")
}

func (m *MemEngine) InsertIdempotent(ctx context.Context, nsName, table string, records []map[string]any, emb Embedder, key string) ([]int64, bool, error) {
	return nil, false, errUnimplemented("idempotent insert")
}

func (m *MemEngine) UpsertByKey(ctx context.Context, nsName, table string, keyFields []string, records []map[string]any, emb Embedder) ([]int64, int, int, error) {
	return nil, 0, 0, errUnimplemented("upsert_by_key")
}

func (m *MemEngine) Update(ctx context.Context, nsName, table, where string, args []any, set map[string]any, emb Embedder) (int64, error) {
	return 0, errUnimplemented("update")
}

func (m *MemEngine) Upsert(ctx context.Context, nsName, table, where string, args []any, set map[string]any, emb Embedder) (UpsertResult, error) {
	return UpsertResult{}, errUnimplemented("upsert")
}

func (m *MemEngine) Delete(ctx context.Context, nsName, table, where string, args []any, opts DeleteOptions) (DeleteResult, error) {
	return DeleteResult{}, errUnimplemented("delete")
}

func (m *MemEngine) Query(ctx context.Context, nsName, query string, args []any, offset, limit int) ([]map[string]any, bool, error) {
	return nil, false, errUnimplemented("query")
}

func (m *MemEngine) SearchFulltext(ctx context.Context, nsName, table, query string, offset, limit int, includeHidden bool, filter string, args []any) ([]map[string]any, bool, error) {
	return nil, false, errUnimplemented("search_fulltext")
}

func (m *MemEngine) SearchVector(ctx context.Context, nsName, table, column string, vec []float32, embedModel string, offset, limit int, includeHidden bool, filter string, args []any, minScore *float64) (VectorSearchResult, error) {
	return VectorSearchResult{}, errUnimplemented("search_vector")
}
