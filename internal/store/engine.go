package store

import (
	"context"
	"fmt"

	"github.com/lsm/dolmen/internal/schema"
)

const (
	EngineSQLite   = "sqlite"
	EnginePostgres = "postgres"
)

func ValidateEngine(name string) error {
	switch name {
	case "", EngineSQLite, EnginePostgres:
		return nil
	default:
		return fmt.Errorf("unknown engine %q (available engines are %q and %q)", name, EnginePostgres, EngineSQLite)
	}
}

type Engine interface {
	NamespaceState(ctx context.Context, ns string, auth []AuthBinding) ([16]byte, error)

	ListNamespaces(ctx context.Context, prefix string, bindings []AuthBinding) ([]string, error)

	CreateNamespace(ctx context.Context, ns string, parentNsGen [16]byte) error

	DropNamespace(ctx context.Context, ns string, nsGen [16]byte) error

	TableState(ctx context.Context, ns, table string, auth []AuthBinding) (*schema.TableSchema, Incarnation, error)

	ListTables(ctx context.Context, ns string, bindings []AuthBinding) ([]string, error)

	CreateTable(ctx context.Context, ns, table string, fields []schema.Field, opts TableOpts, nsGen [16]byte) (*schema.TableSchema, error)

	DescribeTable(ctx context.Context, ns, table string, scope *RowScope, scopeIncarnation Incarnation) (*schema.TableSchema, int64, error)
	Tokenize(ctx context.Context, ns, table, text string, scopeIncarnation Incarnation) ([]string, error)

	DropTable(ctx context.Context, ns, table string, inc Incarnation) error

	PlanMigration(ctx context.Context, ns, table string, changes []schema.Change, emb Embedder, expected Incarnation, scope *RowScope, scopeIncarnation Incarnation) (*MigrationPlan, error)

	Migrate(ctx context.Context, ns, table string, changes []schema.Change, emb Embedder, expected Incarnation) (*schema.TableSchema, error)

	ListMigrations(ctx context.Context, ns, table string, inc Incarnation) ([]Migration, error)

	Insert(ctx context.Context, ns, table string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)

	GetRows(ctx context.Context, ns, table string, ids []int64, scope *RowScope, scopeIncarnation Incarnation) (QueryResult, error)

	UpsertByKey(ctx context.Context, ns, table string, on []string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)

	Upsert(ctx context.Context, ns, table string, filter string, args []any, record map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)

	Update(ctx context.Context, ns, table string, filter string, args []any, set map[string]any, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (UpdateResult, error)

	Delete(ctx context.Context, ns, table string, filter string, args []any, opts DeleteOpts, scope *RowScope, scopeIncarnation Incarnation) (DeleteResult, error)

	ChangesSince(ctx context.Context, ns, table string, from Cursor, nsGen [16]byte, scope *RowScope, scopeIncarnation Incarnation, page Page) ([]ChangeRecord, Cursor, error)

	Listen(ctx context.Context, ns, table string, from Cursor, nsGen [16]byte, liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool), notify func(ChangeRecord), closed func(cause error)) (*ChangeReplay, func(), error)

	Query(ctx context.Context, ns, sql string, args []any, nsGen [16]byte, page Page) (QueryResult, error)

	SearchFulltext(ctx context.Context, ns, table, match string, filter string, args []any, includeHidden bool, scope *RowScope, scopeIncarnation Incarnation, page Page) (SearchResult, error)

	SearchVector(ctx context.Context, ns, table string, q VectorQuery, includeHidden bool, scope *RowScope, scopeIncarnation Incarnation, page Page) (SearchResult, error)

	Capabilities() EngineCapabilities

	Close() error
}

type AuthBinding struct {
	Root         bool
	Ancestor     string
	AncestorGen  [16]byte
	TargetGen    [16]byte
	Table        string
	TableNsGen   [16]byte
	TableDropGen int64
}

type Incarnation struct {
	NsGen   [16]byte
	Table   string
	Version int64
	DropGen int64
}

type RowScope struct {
	Owner string
	Empty bool
}

type WriteOpts struct {
	Owner          string
	IdempotencyKey string
	TableWideRead  bool
}

type TableOpts struct {
	RowAccess string
}

type DeleteOpts = DeleteOptions

type Cursor string

const CursorBegin Cursor = "begin"

type Page struct {
	Offset int
	Limit  int
}

type ChangeKind string

const (
	ChangeInsert ChangeKind = "insert"
	ChangeUpdate ChangeKind = "update"
	ChangeDelete ChangeKind = "delete"
)

type Lifetime struct {
	NsGen   [16]byte
	Table   string
	DropGen int64
}

type ChangeRecord struct {
	Cursor   Cursor
	Table    string
	RowID    int64
	Kind     ChangeKind
	Owner    string
	Lifetime Lifetime
}

type ChangeRange struct {
	First int64
	Last  int64
	Count int64
}

type ChangeReplay struct {
	Next func(ctx context.Context) (records []ChangeRecord, next Cursor, done bool, err error)

	Resume func() Cursor
}

type InsertResult struct {
	Ids      []int64
	Replayed bool
	Inserted int64
	Updated  int64
	Changes  ChangeRange
}

type UpdateResult struct {
	Updated int64
	Changes ChangeRange
}

type QueryResult struct {
	Rows      []map[string]any
	Truncated bool
}

type SearchResult struct {
	Rows           []map[string]any
	Truncated      bool
	SkippedVectors int
	Execution      VectorExecution
}

type VectorQuery struct {
	Column     string
	Vec        []float32
	EmbedModel string
	Filter     string
	Args       []any
	MinScore   *float64
}

type VectorExecution string

const (
	VectorExact VectorExecution = "exact"

	VectorANN VectorExecution = "ann"
)

const (
	DialectSQLite = "sqlite"

	DialectPostgres = "postgresql"
)

type EngineCapabilities struct {
	VectorExecution VectorExecution `json:"vector_execution"`

	ANNRecallBound *float64 `json:"ann_recall_bound"`

	Notifications bool `json:"notifications"`

	Subscribe bool `json:"subscribe"`

	QueryDialect string `json:"query_dialect"`

	FilterDialect string `json:"filter_dialect"`
}

func (s *Store) Capabilities() EngineCapabilities {
	return EngineCapabilities{
		VectorExecution: VectorExact,
		ANNRecallBound:  nil,
		Notifications:   true,
		Subscribe:       true,
		QueryDialect:    DialectSQLite,
		FilterDialect:   DialectSQLite,
	}
}
