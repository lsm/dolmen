# PostgreSQL backend implementation

Implementation started 2026-09-19 at the principal's request. The engine contract
remains [identity-and-engines.md](identity-and-engines.md); this document tracks
concrete implementation decisions and delivery status.

## Native search decision

Each engine executes full-text matching, ranking, and pagination in its database.
SQLite keeps FTS5 with its existing BM25 ordering. PostgreSQL will use an indexed
`tsvector` (GIN) and `ts_rank_cd` descending, then row ID ascending. This is PostgreSQL
native relevance, not BM25. Start with an explicit `english` text-search configuration
and equal field weights; do not inherit a server's ambient default configuration.

The common expression grammar (terms, phrases, AND/OR/binary NOT, prefix) remains the
input contract. Translate that grammar to PostgreSQL tsquery expressions rather than
passing it to websearch_to_tsquery, whose grammar differs. Database-native linguistic
analysis is documented: stemming, accents, stop words, and CJK behavior may produce
different matches. Both matching and relevance changes after migration are accepted.

Preserve result shapes, filters/authorization, bounds, truncation, and deterministic
ID tie-breaking. Test relevance within each backend, not byte-identical cross-engine
ranking. No shared Go BM25 scorer or tokenization extraction is required. The
separate vector score/ANN contract remains in force; vector acceleration needs its
own conformance-backed implementation.

## First implementation increment (implemented internally)

The first increment establishes `internal/postgres` with a pgx connection pool and
namespace catalog. It deliberately
does not yet implement the complete `store.Engine`: there are no placeholder CRUD or
search methods, and `DOLMEN_ENGINE=postgres` / `WithEngine("postgres")` remain rejected.
The namespace conformance subset runs against both implementations; PostgreSQL's
service-backed CI job fails rather than silently skipping when its DSN is missing.

- `Open(ctx, Config)` requires an explicit DSN, validates the catalog identifier,
  connects eagerly, and bootstraps under a transaction-level catalog advisory lock.
  Initialization has a 30-second bound (or the caller's earlier deadline). Connection
  errors retain causes while avoiding connection-string echo in their public message.
- The default catalog schema is `dolmen_catalog`; an internal configurable catalog
  permits independent test installations. Tables carry an explicit catalog version;
  an unsupported version is rejected without committing catalog changes.
- The catalog maps logical namespace paths to physical schemas named from a random
  128-bit generation. Physical identifiers fit PostgreSQL's limit even for a three-part
  path with 64-character segments. Unique constraints and transactional CREATE SCHEMA
  detect collisions; no truncated hash is claimed to be injective. Dropping and
  recreating a logical namespace gives it a fresh generation and schema.
- Create/drop take the same catalog lock to serialize hierarchy changes. Drop locks
  the namespace row, refuses live descendants, and removes schema plus registration
  atomically. In auth-off mode, a child does not require a registered parent, matching
  existing SQLite behavior. Supplied parent/drop generations reject stale lifetimes.
- The private write primitive locks the namespace row until commit. Change ranges use
  a transactional counter on that row, not a PostgreSQL sequence. Allocation rolls
  back with the transaction. Separate namespaces have separate write locks. The
  first tests exercise this primitive with real row writes from two OS processes;
  the durable change-log API itself is a later slice.
- Close rejects new operations and waits for admitted work, then closes the pool.
  Repeated close calls wait for the same completion. Callers bound admitted operations
  with contexts; the adapter does not forcibly terminate application callbacks.
- Nonempty authorization bindings fail closed until the authorization slice exists.
  Revoking PUBLIC schema access is initial hygiene, not a claim that query confinement
  is complete. No caller SQL is exposed in this increment.

Initial CI target: PostgreSQL 17. The configured database role needs CREATE privilege
on the database and ownership of its catalog/namespace schemas. No extension,
superuser, or CREATEDROLE privilege is required for this increment. Role provisioning
for the future confined query path is a separate deployment decision before that path
can ship. Use a dedicated database for tests; the test fixtures create and remove
only uniquely named catalogs and the namespace schemas registered within them.

## Remaining implementation sequence

1. Table/schema lifecycle and registry, shared value coercion/decoding, typed reads.
2. Insert/update/delete/upsert with idempotency and durable change records in the same
   namespace-serialized transaction. Normalize PostgreSQL SQLSTATE errors to dolmen's
   taxonomy; preserve integer and JSON fidelity fixtures.
3. Caller query confinement and schema migrations. Resolve placeholder/operator
   ambiguity and logical table/field-name mapping with parser tests before accepting
   caller SQL. Schema privileges plus catalog/function restrictions are mandatory.
4. Native PostgreSQL full-text indexing/matching/ranking and vector search; add
   per-engine match/relevance fixtures and cross-engine filter/shape tests.
5. Durable cursor replay, retention, cross-process polling/listening, and SSE lifecycle
   tests. Notifications may wake readers but never replace the durable log.
6. Implement every mandatory Engine method, wire the HTTP/MCP/stdio/facade/blackbox
   constructors and complete the conformance matrix; only then enable the public
   selector and publish PostgreSQL configuration/install guidance.

The driver remains pure Go and compatible with the static binary requirement.
PostgreSQL dependency versions are pinned in go.mod. No PostgreSQL server is bundled
with dolmen and no service is started by opening a store.

## Running the current tests

Against a disposable PostgreSQL database:

```sh
export DOLMEN_TEST_PG_DSN='postgres://user:password@127.0.0.1:5432/dolmen_test?sslmode=disable'
export DOLMEN_TEST_PG_REQUIRED=1
go test -race -count=1 ./internal/postgres
go test -race -count=1 ./internal/conformance -run '^TestNamespaceBackendConformance$'
```

Without the test DSN, PostgreSQL integration tests skip during ordinary SQLite-only
local runs. The CI PostgreSQL job supplies both variables and requires real database
execution. Unit tests for configuration validation do not require a database.
