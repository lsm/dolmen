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

## Table lifecycle increment (implemented internally)

The next stacked slice adds CreateTable, TableState, ListTables, DescribeTable, and
DropTable. It shares table-definition/default validation with SQLite and stores logical
schemas with exact JSON number tokens. PostgreSQL uses NUMERIC for number fields,
BOOLEAN for booleans, BYTEA for vectors, and TEXT for strings/timestamps/JSON. Value
coercion and typed row reads are the next slice; these DDL choices do not claim that
raw PostgreSQL casts reproduce SQLite coercion. Application defaults remain in schema
metadata for application during writes, matching the existing SQLite write contract.

Catalog version 2 adds the table registry transactionally; opening a version-1 catalog
upgrades it while preserving namespace generations. Drop tombstones retain table drop
generations and are removed with their namespace. A stale incarnation cannot drop a
replacement table. Schema reads/counts hold a shared namespace-row lock; table changes
hold the exclusive write lock, keeping schema, row count, and incarnation coherent.

Logical-to-physical field/table maps are persisted. Short identifiers normally pass
through; long identifiers use a shortened candidate with a hash suffix. Check for
collisions with other mapped names and PostgreSQL relations (including generated
indexes/sequences), allocate a distinct candidate, and fail rather than alias after a
bounded number of attempts. This is collision detection, not a claim of an injective
truncated hash. The future caller-query path must resolve identifiers with these
persisted maps and preserve caller-visible logical names.

Full-text indexes, row mutations, caller SQL, and notifications are not implemented by
this slice. Selecting postgres publicly remains disabled.

## Shared value handling

`internal/value` contains the existing field coercion and typed decoding rules,
extracted from SQLite without changing accepted inputs, errors, or output types.
SQLite delegates to this package; PostgreSQL row operations will use the same rules
with driver-specific parameter and result conversion. In particular, normalized
booleans currently use integer 0/1 and need conversion to PostgreSQL booleans at
that boundary. SQL default evaluation remains in the adapter.

This extraction preserves signed-integer fidelity, the existing overflow behavior
for JSON number tokens, JSON decoding with `UseNumber`, canonical timestamps,
float32 vector validation/encoding, and base64 fallback for untyped binary values.
The existing facade and transport conformance suites remain the behavior reference.

## Typed row reads (implemented internally)

`GetRows` selects declared fields by their persisted physical names, excludes the
hidden embedding, and returns found IDs once each in ascending order. It preserves
large signed integers, JSON number tokens, booleans, timestamps, and float32 vectors
through the shared value helpers. The ID cap and 32 MiB response budget follow the
SQLite contract, including truncation and rejection of an oversized first row.
Reads hold the namespace lifetime lock and reject stale supplied incarnations.
Nonempty row scopes still fail closed until authorization is implemented.

`Insert` now supplies typed round-trip fixtures in shared SQLite/PostgreSQL
conformance. Direct physical-row fixtures still exercise malformed/oversized data.

## Transactional inserts (implemented internally)

Catalog version 3 adds durable change records and idempotency results. Inserts commit
rows, embedding metadata, the namespace change counter, change records, and retry
results atomically. Change positions serialize across independent processes using the
namespace row lock; PostgreSQL identity sequences assign row IDs only, so aborted
inserts may leave row-ID gaps without consuming change positions.

Retry keys bind to the table lifetime and normalized request body. Matching retries
return the original IDs without generating new changes or calling an embedding
provider. A dropped/recreated table has a fresh retry domain. Authorization options
still fail closed. Timestamp defaults are evaluated at write time.

Embedding work runs outside transactions. Before writing, the adapter rechecks the
namespace/table lifetime and schema, retries schema changes up to three times, and
rejects table replacement. Shared embedding validation rejects mismatched spaces,
invalid dimensions, and non-finite vectors. Durable replay and retention are described below.

## Durable replay and retention (implemented internally)

Catalog version 4 adds opaque, random cursor tokens. `ChangesSince` supports a
current-head cursor, retained-history `begin`, bounded pages, per-event resume, and
stable empty-page cursors. Tokens survive process restart and bind to their namespace,
feed, and table lifetime. Unknown/expired tokens and cross-feed tokens retain the
existing error taxonomy. Supplied namespace/table incarnations are checked.

Retention defaults to seven days. The internal config accepts an optional duration;
explicit zero disables pruning. Successful cursor use refreshes its idle lifetime,
while the replay chain has an absolute twice-retention age bound. Active chains pin
history after their origin; pruning removes only old, unpinned changes and expired
tokens. Pruning runs transactionally with replay and uses the same namespace lock as
writes. Scoped replay remains unavailable until authorization is implemented.

Indexes cover namespace/table change paging and cursor history pins. Cross-process
subscriptions will poll this durable log; no in-memory notification is authoritative.

## Natural-key upsert (implemented internally)

`UpsertByKey` normalizes and validates up to eight scalar key fields, then processes
the batch in caller order while holding the namespace write lock. A later record in
the same batch therefore sees an earlier insert. Concurrent callers serialize at the
namespace boundary, so a previously absent key produces one insert followed by
updates. Existing duplicate keys are reported as a conflict instead of choosing an
arbitrary row.

Inserts apply defaults and required-field checks; updates patch only supplied fields.
Embedding calls run before the write transaction, and the table lifetime and schema
are rechecked before committing. Rows, embedding metadata, and insert-then-update
change records commit atomically. Authorization-bearing options still fail closed.

## Remaining implementation sequence

1. Update/delete and filter-based upsert with durable change records in the same
   namespace-serialized transaction. Normalize PostgreSQL SQLSTATE errors to dolmen's
   taxonomy; preserve integer and JSON fidelity fixtures.
2. Caller query confinement and schema migrations. Resolve placeholder/operator
   ambiguity and logical table/field-name mapping with parser tests before accepting
   caller SQL. Schema privileges plus catalog/function restrictions are mandatory.
3. Native PostgreSQL full-text indexing/matching/ranking and vector search; add
   per-engine match/relevance fixtures and cross-engine filter/shape tests.
4. Cross-process polling/listening and SSE lifecycle
   tests. Notifications may wake readers but never replace the durable log.
5. Implement every mandatory Engine method, wire the HTTP/MCP/stdio/facade/blackbox
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
go test -race -count=1 ./internal/conformance -run '^Test(Namespace|Table|RowRead|Insert|Changes|KeyUpsert)BackendConformance$'
```

Without the test DSN, PostgreSQL integration tests skip during ordinary SQLite-only
local runs. The CI PostgreSQL job supplies both variables and requires real database
execution. Unit tests for configuration validation do not require a database.
