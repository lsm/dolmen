package store

import (
	"context"

	"github.com/lsm/dolmen/internal/schema"
)

// Engine is the seam between the op layer (internal/api) and a storage
// engine. The SQLite-backed Store is adapter #1; DuckDB-over-Iceberg (#164)
// is the second implementation this seam exists for. The signatures are the
// op contract itself — the same shapes internal/api dispatches — so an
// engine owns each operation end to end: request validation, error
// classification, pagination, and the truncated flag.
//
// SQL text arrives as data, not as an engine choice: the read-only
// statement Query takes and the WHERE expressions used as filters are part
// of the public API contract. An engine owns evaluating them under its own
// dialect, allowlists, and index mechanics — the SQLite adapter keeps FTS5
// shadow tables, tokenizers, and PRAGMA DSNs entirely to itself.
//
// Errors classify through the package sentinels and types so the op layer
// maps them to the public error envelope unchanged: ErrInvalid for
// client-correctable requests, ErrNotFound for missing namespaces and
// tables, QueryError for query/filter execution failures, and
// VersionConflictError for migrate preconditions. An unclassified error
// surfaces as internal_error — right for an engine bug, wrong for
// client-fixable input.
//
// Filters stay opaque WHERE expressions composed by the op layer; engines
// receive the composed expression and own only its evaluation (this is the
// hook row-level predicates hang on). Engine lifetime — Close — belongs to
// the constructor site, not the op layer, so it stays outside the
// interface.
//
// Interface sketch coordinated in #159 (item 6); extracted behind the
// existing Store with zero contract change by #76.
type Engine interface {
	// Namespace lifecycle. Namespaces come into existence implicitly on
	// first use; CreateNamespace reserves a name up front and fails when
	// the name is already taken.
	ListNamespaces() ([]string, error)
	CreateNamespace(nsName string) error
	DropNamespace(nsName string) error

	// Table registry and DDL.
	ListTables(ctx context.Context, nsName string) ([]string, error)
	DescribeTable(ctx context.Context, nsName, table string) (*schema.TableSchema, int64, error)
	CreateTable(ctx context.Context, nsName, table string, fields []schema.Field) (*schema.TableSchema, error)
	DropTable(ctx context.Context, nsName, table string) error

	// Schema migrations. expectedVersion is a precondition on the table's
	// current version (0 means unchecked); apply and dry-run share one
	// planner so a preview can never disagree with the real migration.
	ListMigrations(ctx context.Context, nsName, table string) ([]Migration, error)
	PlanMigration(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expectedVersion int) (*MigrationPlan, error)
	Migrate(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder, expectedVersion int) (*schema.TableSchema, error)

	// Row writes. emb is the active embedding provider; engines whose
	// tables vectorize call it on the write and backfill paths.
	Insert(ctx context.Context, nsName, table string, records []map[string]any, emb Embedder) ([]int64, error)
	InsertIdempotent(ctx context.Context, nsName, table string, records []map[string]any, emb Embedder, key string) (ids []int64, replayed bool, err error)
	UpsertByKey(ctx context.Context, nsName, table string, keyFields []string, records []map[string]any, emb Embedder) (ids []int64, inserted, updated int, err error)
	Update(ctx context.Context, nsName, table, where string, args []any, set map[string]any, emb Embedder) (int64, error)
	Upsert(ctx context.Context, nsName, table, where string, args []any, set map[string]any, emb Embedder) (UpsertResult, error)
	Delete(ctx context.Context, nsName, table, where string, args []any, opts DeleteOptions) (DeleteResult, error)

	// Filtered reads and search execution. offset/limit and the returned
	// truncated flag carry the public pagination contract; results and
	// ranking semantics are engine-neutral, while index-versus-scan is the
	// engine's choice (an index is an accelerator, never a semantic change).
	Query(ctx context.Context, nsName, query string, args []any, offset, limit int) ([]map[string]any, bool, error)
	SearchFulltext(ctx context.Context, nsName, table, query string, offset, limit int, includeHidden bool, filter string, args []any) ([]map[string]any, bool, error)
	SearchVector(ctx context.Context, nsName, table, column string, vec []float32, embedModel string, offset, limit int, includeHidden bool, filter string, args []any, minScore *float64) (VectorSearchResult, error)
	ValidateVectorSearch(ctx context.Context, nsName, table, column string, textQuery bool, embedIdentity string) error
}

// The SQLite engine is adapter #1 for the seam.
var _ Engine = (*Store)(nil)
