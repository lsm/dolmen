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

## Caller SQL boundary (implemented internally)

The implementation parses PostgreSQL syntax with the pinned `wasilibs/go-pgquery`
WebAssembly parser, which preserves the no-cgo build. It accepts one SELECT/read-only
WITH statement and validates an allowlist of expressions, built-in functions, types,
and operators. Catalog/schema-qualified references, administrative functions,
SELECT INTO, locking clauses, and mutation CTEs are rejected before execution.

Base tables become generated subqueries exposing declared columns only, with persisted
physical names and logical output labels. The resolver respects CTE scope and aliases.
Identifiers longer than 63 bytes are replaced with collision-checked temporary names
before PostgreSQL parsing, avoiding parser truncation. Placeholders are lexed through
strings, quoted identifiers, nested comments, and dollar-quoted strings. `?` is always
a Dolmen parameter; write `??` for PostgreSQL's JSON existence operator (`?|` and `?&`
remain native operators). Numbered `$N` caller parameters are rejected.

A DBA provisions one restricted NOLOGIN query role per dedicated Dolmen database and
grants the backend account permission to `SET ROLE` to it. Dolmen does not need
CREATEROLE. The backend grants that role schema USAGE and SELECT on declared columns
only; hidden embeddings and catalog tables are not granted. New tables receive grants
transactionally. Execution switches to the restricted role inside a read-only
transaction with a 30-second statement timeout. Rollback resets connection-local
settings before pool reuse. Parser validation additionally blocks PostgreSQL's
PUBLIC-readable catalog and function-equivalent probes. A dedicated database keeps the
shared query role's grants within one Dolmen installation; parser rewriting enforces
the logical namespace boundary.

The exact built-in allowlists live in `internal/postgres/sql_parse.go`. This native
PostgreSQL SQL surface is not a SQLite SQL translator. Integration tests cover logical
names, typed results, pagination, response bounds, role isolation, and pooled-state
cleanup. Tests use the same pre-provisioned role model as production.

## Filter mutations (implemented internally)

Update, delete, and filter-based upsert compile their filter as a confined single-table
`SELECT id` through the same PostgreSQL parser boundary. This preserves logical field
names and `?` arguments while rejecting additional statements, schema-qualified
references, and unsupported functions or operators. Matching IDs are selected and
mutated under the namespace write lock, with row changes and durable change records in
one transaction.

Updates patch only supplied fields. Filter upsert updates every match or inserts one
record with defaults and required-field validation when there is no match. Delete keeps
the existing dry-run, match limit, and explicit confirmation contract. Embedding work
runs before the write transaction and is skipped for no-match updates; invalid fields
and values are still rejected even when a filter matches nothing.

## Schema migrations (implemented internally)

Migrations validate the same six `schema.Change` ops as SQLite, in request order, and
reproduce its plan output and rejection messages: the field cap, required-without-
backfill, enum values still stored by rows, the single-vectorized-field rule, and the
`expected_version` requirement for destructive changes. `PlanMigration` runs the same
validation and probes without writing. `Migrate` plans twice: once to decide what
embedding work is needed, and again inside the namespace write transaction, whose plan is
the one applied. Re-planning under the lock re-runs the data probes, not just the
incarnation check, so a value written between the two plans — an enum member a concurrent
insert added that the new vocabulary excludes — is rejected rather than committed against
the schema that forbids it. `checkIncarnation` supplies the version compare-and-set and
returns the shared `VersionConflictError`.

Catalog version 5 adds a `migrations` relation keyed by namespace, table, drop
generation, and a per-table id, so `list_migrations` returns newest-first history with
the same shape as SQLite and a recreated table starts a fresh log. Schema JSON, the
physical column mapping, and the history row are written in the transaction that runs
the DDL, so a failed step leaves version, columns, and history untouched.

Physical column names persist in `columns_json` and are allocated incrementally: an
existing field keeps its stored physical name across unrelated migrations, a new field
gets a collision-checked name, and a rename renames the physical column so short
logical names keep matching their column. Pre-DDL probes (enum scans, full-text and
embedding estimates) read the pre-migration physical name, because the new name does
not exist until the DDL runs. Migrations re-issue the query role's column grants so
caller SQL sees added fields and loses dropped ones.

Embedding backfills run outside the write transaction. The migration pages the vectorized
column in id order under short read transactions, embedding each batch with no lock held,
then applies the DDL and the precomputed vectors under the write lock, confirming each
row's text is unchanged by comparing a digest rather than retaining the text. Paging keeps
the working set per batch bounded; the vectors themselves are held until the write, which
is the price of applying the whole backfill in one transaction. If a concurrent write moved a row, the transaction rolls back and the
migration retries; no embedding call happens while the namespace write lock is held. A
newly added vectorized field with a constant backfill default embeds that text once.

Full-text changes record schema state and report the same `rebuild_fulltext` and
`fulltext_reindex_rows` plan fields as SQLite, but build no index yet: PostgreSQL-native
full-text indexing is the next increment, and it takes over the index work without
changing these plan semantics.

## Native search (implemented internally)

Full-text search uses a STORED generated `tsvector` column over the table's fulltext
fields with a GIN index, matched with `@@` and ordered by `ts_rank_cd` descending then
id ascending. The text-search configuration is the explicit `english` one, never the
server's ambient default, and fields carry equal weight. Relevance is PostgreSQL-native
and does not agree with SQLite's BM25 ordering; ranking is tested within each backend,
not across them.

The match grammar is translated to a tsquery expression rather than passed through.
Terms become `plainto_tsquery`, phrases `phraseto_tsquery`, and prefixes a quoted
lexeme with `:*`, combined with the `&&`, `||`, and `!!` tsquery operators. Every term
and phrase reaches PostgreSQL as a bind parameter, so tsquery metacharacters in a query
become literal lexemes instead of operators. Two pieces of the FTS5 grammar have no
faithful equivalent and are rejected with a message naming the alternative rather than
mistranslated: the `field:term` column filter (and its `{field field}:term` group form),
`NEAR()`, the `^` first-token operator, and `+` adjacency. The last two matter because
they would otherwise pass through as term text — PostgreSQL drops a leading `^` and
matches the word anywhere, and a lone `+` compiles to an empty query matching nothing —
so silently returning different rows than FTS5 rather than refusing. Stemming, stop words, and accent handling are PostgreSQL's, so a query of
only stop words matches nothing where FTS5 would match.

Migrations drop the generated column before the field DDL and rebuild it after, because
PostgreSQL refuses to drop a column a generated column reads.

The index name is allocated through the same `pg_class` probe that table names use, not
derived as `<table>_fts`. Indexes and tables share one namespace in PostgreSQL, and the
grammar reserves only the `__fts` substring, so a namespace may legitimately hold a table
called `notes_fts`; deriving the name would make full-text on `notes` fail with 42P07 and
stay broken.

Vector search scans stored vectors and scores cosine similarity in Go, exactly as SQLite
does, and reports `exact` execution. Ordering, `_score`, `min_score`, skipped-vector
counting, filters, and paging agree with SQLite row for row, and the conformance suite
pins that. No PostgreSQL vector extension is required; approximate-nearest-neighbour
acceleration remains a separate increment with its own conformance-backed contract.

Both searches apply their filter through the same confined parser the mutations use, so
a filter sees declared columns only and cannot reach the generated tsvector column or
hidden embeddings. Result shapes, typed coercion, hidden-column behaviour, response
budgets, and truncation match the other read paths.

## Cross-process subscriptions (implemented internally)

`Listen` is built on the durable change log, not on notifications. A subscription
resolves its starting cursor exactly as `changes_since` does — a token, `begin`, or the
current head — replays through `ChangeReplay.Next` until a page comes back empty, then
polls the same log for live changes. Because every record comes from a committed row in
the catalog, a subscription sees writes from any process against the same database, and
two store instances over one catalog are covered by an integration test.

Replay and live delivery never overlap. The session's poller does not begin until
`ChangeReplay.Next` reports the replay drained, and every read of the feed — from the
replay or from the poller — takes the session lock for the whole cursor-read, fetch, and
cursor-write span. Two reads can therefore never start from the same cursor, so a change
is delivered exactly once across the boundary rather than once by `Next` and again by
`notify`.

`pg_notify` is a latency optimisation layered on top. Writes announce their namespace on
a per-catalog channel, and one `LISTEN` connection per store fans the wake-up out to the
sessions for that namespace; a session that is woken simply polls earlier. A poll interval
runs regardless, so a dropped, missed, or disabled notification costs latency and never a
change: a test removes the wake registration entirely and still requires the change to
arrive. That connection is opened directly rather than taken from the pool, because a
pooled connection parked in `WaitForNotification` would hold a slot for the store's
lifetime and deadlock writes on a small pool. The channel name is length-capped the same
way physical identifiers are, since a 63-character catalog would otherwise push it past
PostgreSQL's 63-byte limit, and the announce runs in a savepoint so a failed notification
— a full notification queue, say — cannot poison the write transaction that raised it.
Notifications are an optimisation, and a write must not fail because one did.

A cursor is validated at `Listen` rather than on the first page, so a token minted on
another feed is refused up front with the cross-feed error and its own remediation instead
of surfacing later as an age bound. `Listen` also anchors the feed there, minting a cursor
at the resolved head or retention floor inside its own transaction, so a commit landing
between `Listen` returning and the first `Next` is delivered rather than falling below a
boundary resolved later.

`ChangeReplay.Next` serializes against the session and refuses calls once the subscription
is cancelled. A context cancelled by the caller for one `Next` call ends that call only:
the error is returned but the session stays usable, since a per-call deadline is not a
statement about the subscription.

A subscription re-anchors its own cursor once every quarter of the retention window,
minting a fresh chain at its current position. Two bounds make this necessary. A token is
expired once it has gone unrefreshed for a retention window, and a chain is expired
outright at twice retention, so a feed quiet for longer than that would expire its own
cursor, lose the next change, and hand back a resume cursor that no longer resolves.

That re-anchoring lives in the subscription, not in `changes_since`. The absolute chain
bound is deliberate for the polling surface — a caller holding one chain forever is made
to re-anchor — and `TestPostgresCursorDurabilityAndRetention` pins it. Only a live
subscription, which cannot ask its caller to reconnect without dropping the stream, is
exempt. The cost is one write per subscription per quarter-window, not the one per poll
that the read-only guard replaced.

The subscription captures the namespace generation and the table's drop generation at
`Listen` rather than trusting caller-supplied bindings, so a dropped and recreated table
or a replaced namespace ends the feed even when the caller passes a zero incarnation. The
schema version is deliberately not captured: a migration must not end a subscription.

Every poll tick first runs a read-only guard: it re-checks `liveAuthz`, which is invoked
from the session goroutine and so must be safe to call concurrently, and re-reads the
namespace generation and table drop generation. Only when that guard passes and a
read-only probe finds changes past the cursor does the tick take the write path that mints
cursors, so an idle subscription costs two reads rather than a write transaction
contending with writers on the namespace row. Records are additionally checked against
`liveAuthz` per record, so a namespace-wide feed re-checks per table rather than once. A
`liveAuthz` that returns a row scope fails closed, as every other PostgreSQL operation
does.

Ends are reported through `closed` with the shared sentinels the transports match on —
`ErrListenRevoked` for a withdrawn admission, `ErrListenLifetimeEnded` for a replaced
target or a closing store, and `ErrListenAged` for a cursor past retention. A closing
store is checked before each tick's work and again if that work fails, so a session
racing `Close` reports the sentinel rather than whatever error the closing pool happened
to raise. Both the `closed` and `notify` dispatches recover from a panicking callback and log
it: they run on the engine's session goroutine, so one bad subscriber would otherwise take
the process down. A panicking `notify` is logged and the subscription continues; only the
record that raised it is lost.

Replay admits per record too, not only the live phase, so a namespace-wide feed whose
admission is withdrawn during a long replay stops rather than finishing the backlog. Plain
cancellation reports nothing: the transports treat the close cause as an error to render,
and a nil cause is not one. The session is bound to the
caller's context, so cancelling that context ends it. A terminal error during the replay
phase — an expired cursor, a withdrawn admission, a target that went away — reports
through `closed` just as a live one does, rather than only surfacing as the error returned
from `Next`, so a transport waiting on the close signal is never left hanging.

Cancelling is idempotent, and it joins the session goroutine so no delivery can follow it
— except while a terminal callback is running, where it returns without joining. `closed`
runs on the session goroutine, so joining from inside it would wait on the goroutine that
is waiting on the callback.

## Remaining implementation sequence

1. Close the conformance gaps listed under "Conformance matrix status" below, then wire
   the HTTP/MCP/stdio/facade/blackbox constructors, enable the public selector, and
   publish PostgreSQL configuration/install guidance.

## Conformance matrix status

The conformance suite runs against either backend. `DOLMEN_ENGINE=postgres` selects
PostgreSQL for the whole package; the knob is read by the suite's own engine resolver,
not by `store.ValidateEngine`, so the public selector stays closed while the matrix is
still red:

```sh
DOLMEN_ENGINE=postgres DOLMEN_TEST_PG_DSN=... DOLMEN_TEST_PG_QUERY_ROLE=dolmen_query \
  go test ./internal/conformance
```

Each harness derives a catalog schema from its data directory, so a harness restart
reconnects to the same catalog instead of a fresh one. Three groups skip deliberately:
auth-on harness modes (row authorization is unimplemented on PostgreSQL), the embedded
facade fixtures and the stdio subprocess fixtures (neither constructor can select the
engine yet), and fixtures that probe SQLite storage internals directly.

Listen anchors a replay boundary at the namespace head, so replay terminates under
concurrent writes and the stream reaches its ready frame, and it rejects a cursor that
is past the retention window before opening a stream rather than after.

The live phase has its own cursor, minted at that same head in the same transaction, and
starts when the subscription is registered rather than when replay drains — matching the
SQLite session, whose live pump starts at registration. Live records land in a queue
bounded at `8 * MaxChangesPageLimit`, the SQLite bound; a subscriber that stops draining
overflows it and ends with `ErrListenOverflow` instead of back-pressuring forever.

Query rejection now shares one contract across backends: `store.ValidateQueryShape`
applies the SELECT/WITH and multiple-statement checks before either engine parses, and
SQL-content rejections carry `store.QueryError` so they classify as `query_error` while
request-shape rejections stay `invalid_request`.

One failure is **not** a gap and must not be "fixed" in the backend.
`TestTypedReadEmbeddingHidden` requires an explicit `SELECT _embedding` to return the
vector. On PostgreSQL caller SQL runs as the restricted query role, and
`TestPostgresQueryNamespaceBoundary` pins `SELECT _embedding` as a rejected query
alongside `pg_read_file` and a `DELETE ... RETURNING`, and separately pins that the
query role cannot read the column even through raw SQL. Exposing it — by adding the
column to the compiled projection and to the role's column grant — makes the
conformance fixture pass and breaks that containment test. The README and the skills
already describe `_embedding` caller-SQL access as backend-dependent for this reason,
so the fixture needs an engine-aware pin, not a wider query role.

The remaining failures are genuine backend gaps, not harness artifacts. They must be
closed before the public selector is enabled:

| Area | Fixture | Gap |
|---|---|---|
| Error taxonomy | `TestGoldenErrorContract` | codes now match; the unknown-column and malformed-filter messages still diverge from the pinned shapes, and the three full-text rows pin FTS5 wording that D27 makes per-engine, so they need an engine-aware pin |
| Error taxonomy | `TestTransportParityErrorEnvelope` | a rejected query answers `200` with an empty result set instead of `400` |
| Error taxonomy | `TestDropNamespaceNotFoundDoesNotAdviseCreating`, `TestSearchFulltextFilterArgs` | remediation wording diverges from the pinned shapes |
| Typed reads | `TestTypedReadAliasesAndFallbacks` | an alias to an undeclared label reads back as boolean `true` rather than `1` |
| Full-text | `TestSearchFulltextSyntaxAcceptReject` | diacritic-insensitive matching returns nothing; needs `unaccent` or a documented divergence under D27 |
| Change feed | `TestChangesSinceTableFeedContract` | a live table-feed cursor is rejected as past the retention window |
| SSE | `TestSubscribeOverflowTeachesReconnect` | the bound itself now exists and `TestPostgresListenOverflowsABlockedSubscriber` pins it, but the fixture parks a consumer and floods 9000 changes, which needs the live pump to get 8000 ahead of the writer. Live fetches go through `ChangesSince`, which takes the namespace write lock to mint a cursor per record, so the pump contends with the very writer it must outrun and no backlog accumulates. Closing this means a live read that does not take the write lock |

The driver remains pure Go and compatible with the static binary requirement.
PostgreSQL dependency versions are pinned in go.mod. No PostgreSQL server is bundled
with dolmen and no service is started by opening a store.

## Running the current tests

Against a disposable PostgreSQL database:

```sh
export DOLMEN_TEST_PG_ADMIN_DSN='postgres://admin:password@127.0.0.1:5432/dolmen_test?sslmode=disable'
psql "$DOLMEN_TEST_PG_ADMIN_DSN" -c "CREATE ROLE dolmen_query NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; CREATE ROLE dolmen_backend LOGIN PASSWORD 'backend-test'; GRANT dolmen_query TO dolmen_backend WITH INHERIT FALSE, SET TRUE; GRANT CONNECT, CREATE ON DATABASE dolmen_test TO dolmen_backend"
export DOLMEN_TEST_PG_DSN='postgres://dolmen_backend:backend-test@127.0.0.1:5432/dolmen_test?sslmode=disable'
export DOLMEN_TEST_PG_QUERY_ROLE=dolmen_query
export DOLMEN_TEST_PG_REQUIRED=1
go test -race -count=1 ./internal/postgres
go test -race -count=1 ./internal/conformance -run '^Test(Namespace|Table|RowRead|Insert|Changes|KeyUpsert|Query|Mutation)BackendConformance$'
```

Without the test DSN, PostgreSQL integration tests skip during ordinary SQLite-only
local runs. The CI PostgreSQL job supplies both variables and requires real database
execution. Unit tests for configuration validation do not require a database.
