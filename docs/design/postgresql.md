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
search methods, and `DOLMEN_ENGINE=postgres` / `WithEngine("postgres")` remained rejected
at that point; the facade selector is described under "Selecting PostgreSQL" below.
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

Catalog version 6 gives the retry record an owner, so a key belongs to the principal who
used it rather than to the table. The domain is the writer's principal, which is empty
under `auth: off` and so keeps the shared v0.2.0 domain; a caller holding table-wide read
falls back to that empty domain after missing its own, which is how records written before
owners existed stay replayable to whoever may read the whole table. That is the same rule
SQLite applies, from the same `store.DomainFor`, rather than a second copy of the policy.
The records move to a new relation rather than the old one gaining a column, which is
what SQLite does and for the same reason: the catalog version is checked when a process
opens, not per operation, so a version 5 process already holding the catalog would keep
querying without an owner. Against a widened relation its lookup matches several owners'
rows and returns whichever comes first. Against a dropped one it fails, which is the
answer a process reading a catalog it no longer understands should get.

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
references, and unsupported functions or operators. Confined to a single table means
exactly that: a filter naming another table answers `not_found`, so a cross-table
subquery that SQLite accepts under `auth: off` does not compile here. The scoped filter
allowlist (§4.3) forbids the same thing under `auth: on`, so the engines converge there
and diverge only in the unauthenticated language — recorded as D32 in the governing spec,
since §8.1's byte-for-byte rule had to be read as binding the engine v0.2.0 shipped on
rather than every future engine. The divergence runs both ways: because a mutation filter
compiles through the same boundary as `query`, this engine also **accepts** filter syntax
SQLite refuses (`now`, `md5`, `regexp_match`, the JSON and regex operators), and shared
spellings can differ in meaning — `LIKE` is case-sensitive here and ASCII-case-insensitive
on SQLite. Naming the dialect on the capability surface is #386. `internal/conformance` pins both halves in one fixture
rather than skipping either: PostgreSQL must answer `not_found`, SQLite must still execute
the subquery. Matching IDs are selected and
mutated under the namespace write lock, with row changes and durable change records in
one transaction.

A scoped call confines its filter the way §4.3 requires: the visible rows are selected
into a materialized `_dolmen_visible` relation first and the caller's expression is
compiled against that relation, never the base table. A filter that raises on a foreign
row therefore cannot be used as an oracle over rows the caller cannot read, because the
expression never evaluates on them. Fulltext and vector search confine the same way.
What remains of §4.3 is the shared evaluator rather than the shared validator: a scoped
filter still executes with this engine's own function set, so a spelling SQLite accepts
and PostgreSQL does not (the date and time functions) is a `query_error` here. `iif` no
longer belongs on that list: it renders as a `CASE`, and only a wrong argument count is
refused. That convergence is #386.

### Comparison affinity

A scoped comparison follows SQLite's comparison-affinity rules, not its storage-class
ordering. Storage-class ordering — numbers before text before blobs, and text never
equal to a number — only decides a comparison when *neither* side has a declared
affinity, which in practice means both sides are literals or bound arguments. When one
side is a column the rules convert the other side first: a text column compared against
a number takes the number's text form, so `code = 1` is true of the text `'1'` and
`mark > 1` is false of `'!zzz'`; a number column compared against text converts the text
when it parses as a number and leaves it as text when it does not, so `n = '-7'` is true
of `-7` while `n > 'abc'` stays a cross-class comparison. Column values that are only
known at runtime get the conversion as a SQL `CASE` over the same numeric shape.

The trap this hides in is fixture choice. A fixture value like `'a note from alice'`
answers the same under both rules, so a pinned case built on it passes whichever rule
the engine implements and pins nothing. The conformance fixture carries `code`, `mark`
and `huge` for exactly this reason: each was chosen because the two rules disagree about
it.

Converting a number to a column's text affinity only happens for a value known while
rendering: a literal, a bound argument, or a sign applied to either. SQLite takes the
text of the double, so `-1e300` is `'-1.0e+300'` and `round(2.5)` is `'3.0'`, and
PostgreSQL's own numeric formatting reproduces neither. A computed number compared
against a text column is therefore refused with the usual advice to bind the value,
because the alternative is a wrong row set on a filter that drives `delete`. If that
refusal ever becomes an answer, this paragraph goes with it: the test that pins the
refusal fails at that moment and says so. Numeric
text converted the other way is rounded through a double first, the way SQLite's numeric
affinity does, so `'0.10000000000000000001'` matches a stored `0.1`; an integer that
fits in 64 bits keeps its exact digits instead.

Division truncates only when both operands are integers in SQLite's storage sense, which
is not the same as being numerically whole. A literal written `7.0` or `1e1` is REAL, so
`7.0 / 2` is `3.5` and not `3`, and text converts the same way: `'7.0'` is REAL where
`'7'` is INTEGER. A stored number is the case where whole really does mean integer,
because the SQLite column is `NUMERIC` and stores a lossless `7.0` as INTEGER `7`, so a
column keeps the runtime test while a literal or a bound argument is classed while
rendering.

Coercing text to a number keeps a null null. Text with no numeral at its head converts
to zero, which a `COALESCE` expresses, but a null column is not text with no numeral in
it: SQLite answers `NULL + 1` with null and `'abc' + 1` with one. Conflating them makes
`NOT body` match a null row that SQLite skips and `body + 1 IS NULL` skip one it matches,
so the conversion tests the operand for null before the `COALESCE` rather than after.

Text coerced to a number saturates the way SQLite's double does rather than raising.
`abs(body)` over a text `'1e999999'` is infinity, matching SQLite, where a bare
`::numeric` cast raises `22003` and kills the statement; an overflowing literal such as
`abs(1e999999)` saturates at render time. The runtime guard is `pg_input_is_valid`,
which requires **PostgreSQL 16 or newer** — the first hard lower bound this adapter
places on the server version.

Anything in a boolean position is rendered as SQLite's truth value, at the top of the
expression and under `NOT`, `AND` and `OR` alike. A number is true when it is nonzero, and a
text is converted to a number first so `'a note'` is false and `'1'` is true. A blob goes
the same way through its own bytes read as text, so `X'6162'` is false because `ab` is
zero while `X'31'` is true because `1` is one — a blob is not simply false. Text is not only a column or a literal: concatenation, a `CASE`, and `iif`,
`coalesce`, `ifnull` and `nullif` over text operands all carry text affinity, and a
truth test over one of those converts before testing rather than asking PostgreSQL to
compare text against zero. A boolean column and a bound boolean argument are already boolean and are used
as they are: PostgreSQL has no `boolean <> numeric` operator, so wrapping them the way a
number is wrapped raises `42883` on a filter as ordinary as `flag`.

A boolean column is an integer everywhere except a boolean position. SQLite stores one
as an integer, so `flag = 1`, `flag + 1`, `abs(flag)` and `flag || 'x'` all answer there,
while PostgreSQL holds a real `boolean`. The column therefore renders as `(col)::int` by
default, and only a boolean position takes it raw: a truth test, and a comparison whose
other side is itself boolean-shaped, so `flag = TRUE` and `flag = (n > 1)` still compare
as booleans. The condition of an `iif` and of a `CASE` without an operand is a truth test
too, which is what lets `iif(flag, ...)` and `iif(1, ...)` work.

`IN`, `BETWEEN` and `IS` apply the same rules, because they are rendered through the
same comparison. `x IN (a, b)` becomes `x = a OR x = b`, `x BETWEEN lo AND hi` becomes
`x >= lo AND x <= hi`, and each of those comparisons converts affinity on its own. `IS`
is the null-safe one, so it renders `COALESCE(x = y, FALSE) OR (x IS NULL AND y IS
NULL)`: the equality carries the affinity conversion, and the second arm restores the
"both null is a match" rule the `COALESCE` would otherwise lose. An operand that is a
bound argument is cast from its Go value before it reaches an `IS NULL`, because
PostgreSQL cannot infer a bare placeholder's type there.

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

The HTTP, MCP, stdio, facade, and blackbox constructors all select PostgreSQL, and the
public selector is enabled on both the binary and the Go API. Operator-facing guidance
lives in [postgresql-operations.md](../postgresql-operations.md); this document stays
the design record. The conformance matrix is clean and CI keeps it that way; see
"Conformance matrix status" below.

Nothing in the original sequence is outstanding, and row authorization is implemented:
the auth-on conformance modes run on this backend rather than skipping.

A table declaring `row_access` carries an `owner text` column, which every insert path
stamps from `WriteOpts.Owner` and no caller can supply. The column reads back beside
`id` and `created_at`, and a `RowScope` becomes an `owner = $n` conjunct on row reads,
the `describe_table` count, and both searches; an empty scope is the constant `false`,
so a schema-only holder counts zero rather than seeing a real total. The scope's
incarnation is re-checked inside the operation's transaction and fails `conflict` on a
mismatch — the guard binds even to a nil scope, so a request resolved before
`row_access` was enabled cannot execute as unscoped afterwards.

Where SQLite refuses a scoped call rather than filtering it — `upsert_by_key`, the change
feeds, and migration plans — this backend refuses it with the same sentence. Those messages are now exported from
`internal/store` rather than retyped here, because the conformance suite pins the wording
and two copies would drift.

Two divergences surfaced while wiring this up, both of which had been pinned as correct
by engine tests written while row authorization was unimplemented. A stale scope
incarnation answered `not_found` where SQLite answers `conflict`, and a scope on a table
with no owner column answered `forbidden` where SQLite answers `invalid_request`. Both
now match SQLite, verified against it directly rather than by reading its code.

Authorization bindings stay fail-closed here, which is deliberately stricter than SQLite,
where they are ignored. Every call site passes nil today, so the strictness costs nothing;
silently ignoring an authorization argument is the wrong default for the day one is passed.

## Selecting PostgreSQL

`store.ValidateEngine` accepts `postgres`. The driver is reached through a first-party
subpackage rather than from the facade itself:

```go
import (
    "github.com/lsm/dolmen"
    "github.com/lsm/dolmen/postgres"
)

st, err := dolmen.Open("", postgres.With(postgres.Config{
    DSN:       "postgres://dolmen_backend@host:5432/dolmen?sslmode=disable",
    Catalog:   "dolmen_catalog",
    QueryRole: "dolmen_query",
}))
```

The subpackage exists to keep the dependency off everyone else's build. Importing
`internal/postgres` from the root package put pgx and the embedded WASM PostgreSQL
parser into the import graph of every consumer of the module: the external-module
example grew from 13.1 MB to 35.3 MB whether or not it used PostgreSQL. `postgres.With`
returns a `dolmen.Option` carrying an engine opener, so only programs that import the
subpackage link the driver, and the example is unchanged at 13.8 MB.

`WithEngine("postgres")` without that option is refused with an error naming the import
to add. A DSN is required: `Open` never reads one from the environment, matching the
facade's rule that it reads no configuration of its own.

The data directory argument belongs to SQLite and is unused here, so pass `""`. One
live store per catalog per process is still enforced, keyed on catalog plus the
connection's host, port, database and user as pgx parses them rather than on the DSN
text, so two spellings of the same target collide instead of opening two pools over
one catalog. Supplying a connection and then contradicting it with `WithEngine` is
rejected by name rather than failing later as a connection error.

The binary selects it the same way:

```sh
dolmen -engine postgres \
  -pg-dsn 'postgres://dolmen_backend@host:5432/dolmen?sslmode=disable' \
  -pg-catalog dolmen_catalog -pg-query-role dolmen_query
```

`DOLMEN_PG_DSN`, `DOLMEN_PG_CATALOG` and `DOLMEN_PG_QUERY_ROLE` are the env
equivalents. `-engine postgres` without a DSN is refused, and a DSN without that
engine is refused too, so a half-configured server fails at startup rather than
silently serving SQLite. The stdio conformance fixtures run the real binary against
PostgreSQL through these flags.

`-data` still matters under PostgreSQL. No table data lands there, but the grant
registry (`<data>/_grants.db`) and the local embedding model cache (`<data>/models`)
do, so the binary creates the directory whichever engine serves the tables. It used
to be created as a side effect of opening the SQLite store, which meant `-auth on`
against a missing directory failed at the grant registry under PostgreSQL and
nowhere else.

The role provisioning is the deployment's, not dolmen's: the backend role needs CREATE
on the database, and caller SQL needs the pre-provisioned restricted query role granted
to it with `INHERIT FALSE, SET TRUE`. No runtime `CREATEROLE` is required and no
extension is needed.

## Conformance matrix status

The conformance suite runs against either backend. `DOLMEN_ENGINE=postgres` selects
PostgreSQL for the whole package. The knob is the suite's own, read by its engine
resolver rather than by `store.ValidateEngine`, which is what lets the suite name an
engine without going through the same validation the public selector uses:

```sh
DOLMEN_ENGINE=postgres DOLMEN_TEST_PG_DSN=... DOLMEN_TEST_PG_QUERY_ROLE=dolmen_query \
  go test ./internal/conformance
```

Each harness derives a catalog schema from its data directory, so a harness restart
reconnects to the same catalog instead of a fresh one. The stdio subprocess fixtures
drive the real binary against PostgreSQL through its own flags, and the embedded facade
fixtures derive their catalog the same way and open through `postgres.With`, so both
constructors are exercised rather than skipped. Two groups skip deliberately: auth-on
harness modes (row authorization is unimplemented on PostgreSQL) and fixtures that probe
SQLite storage internals directly.

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

Filters share the query contract's remediation. A `;` in a filter is rejected with the
wording SQLite uses instead of reaching the parser, and a filter that fails to parse is
reframed: the compiler wraps a filter into `SELECT id FROM t WHERE <filter> ORDER BY
id`, so a malformed filter surfaced a syntax error naming `ORDER` — a token the caller
never wrote, from a statement they cannot see. Only parse failures are reframed, so an
unknown table or function still names what was missing; an unknown filter column
parses cleanly and fails at execution, where it keeps the generic remediation pointing
at `describe_table` rather than naming the column. A failed `drop_namespace` now
says nothing was dropped rather than pointing at `list_namespaces`.

A cursor is bound to the namespace lifetime that minted it, and that check stays: a
predecessor cursor against a recreated namespace is a lifetime error, as the engine
design requires. Table lifetimes are not the same rule. PostgreSQL also stored the
table's drop generation in the cursor and rejected a mismatch, so a cursor minted
before a table was dropped and recreated failed on the successor's feed. Nothing in
the engine design asks for that, and the records are already filtered to the current
drop generation, so the predecessor's rows cannot leak: the cursor's position simply
carries over and the caller sees the successor's own events, which is what SQLite does
and what the shared fixture pins. `TestPostgresCursorPinsHistoryAndLifetime` asserted
the rejection and now asserts the successor behavior instead; its namespace-replacement
assertion is unchanged. Subscriptions are unaffected — they refuse to cross a table
lifetime through `checkIncarnation` and the live guard, not through the cursor's
stored generation. `changesSince` runs `checkIncarnation` against the session-pinned
incarnation before it resolves the cursor, so a live fetch after a drop and recreate
ends the subscription instead of adopting the successor's generation.

A bare `-` is now rejected by the PostgreSQL query translator as it is by FTS5. It is
not in the common grammar, and PostgreSQL's parser treats it as punctuation, so such a
query was accepted and answered `200` with an empty result set where the contract
requires a teaching `400`.

The fixtures now carry those engine-aware pins rather than leaving them red. The golden
error contract keeps its SQLite expectations and consults `postgresErrorPins` for the
full-text rows, so both engines are asserted to teach the same remediation in their own
vocabulary — a rejected query still has to be rejected, with the same status and code,
and only the wording differs. Accent folding, the raw read of a boolean under an
undeclared label, and `_embedding` through caller SQL are each asserted per engine, so
PostgreSQL's behavior is pinned rather than merely tolerated: caller SQL must be refused
the embedding column, not just happen to fail. One assertion unions a boolean with a
string under a single label, which only a dynamically typed engine can answer; it is
marked SQLite-only beside the existing blob-cast fixture.

Two more failures are per-engine differences the design already grants, not gaps.

`TestTypedReadAliasesAndFallbacks` expects an alias to an *undeclared* label to read
back raw as `1`. Raw means whatever the engine stored, and this document specifies
BOOLEAN for boolean fields, so PostgreSQL's raw value is `true`. Coercing it to `1`
would also misreport genuine boolean expressions — `SELECT n > 5 AS big` is `true` in
PostgreSQL and `1` in SQLite — so the fixture is pinning SQLite's storage
representation and needs an engine-aware expectation.

`TestSearchFulltextSyntaxAcceptReject` expects `cafe` to match `café latte`. The
native search decision above lists accents, alongside stemming, stop words and CJK,
as analysis that may produce different matches per engine (D27). Matching PostgreSQL
to SQLite here needs the `unaccent` extension, and this document requires that no
extension be needed to run the backend.

Live admission is per record, not per subscription. The authorization callback returns
the incarnation its decision was resolved against, and a table-filtered feed compares
that against each record's `Lifetime`, so a drop and recreate between the callback and
admission cannot carry a stale decision onto the successor's records. A mismatch filters
the record; only `ok=false` ends the stream. Namespace-wide feeds compare `nsGen` only,
since their replay spans table lifetimes by design. No transport passes a callback yet,
so this is unreachable from the conformance suite and is pinned by engine tests.

Dropping the namespace row's exclusive lock from the live read has one consequence
worth naming: a concurrent `drop_namespace` can now interleave with the cursors a
fetch is inserting, and the foreign key from `cursors.namespace` answers with
SQLSTATE 23503. That is the subscription's target disappearing, so `listenCause` maps
it to `ErrListenLifetimeEnded` rather than letting a driver error reach the close
frame. The session was over either way; the caller is told why in the vocabulary the
rest of the contract uses.

Dropping the lock also inverted the lock order, which is a deadlock rather than a
misreport. The fetch updates the cursor row it resolved and only afterwards inserts the
page's cursors, whose foreign key takes `KEY SHARE` on the namespace row;
`DropNamespace` takes the namespace row exclusively first and its `ON DELETE CASCADE`
then wants those same cursor rows. Each side ends up holding what the other needs, and
PostgreSQL breaks the cycle with SQLSTATE 40P01 — surfacing either as a driver error on
the subscription's close frame or as a `drop_namespace` that fails for no reason the
caller can act on. Reproduced against a live server before fixing.

The order is now the same on both paths: the live read takes `FOR KEY SHARE` on the
namespace row, so a dropper waits behind it instead of overtaking it and cascading into
the cursors the fetch holds. That alone would have handed the contention straight back,
because writers took the namespace row `FOR UPDATE`, which `KEY SHARE` conflicts with.
Writers now take `FOR NO KEY UPDATE`: still mutually exclusive, still exclusive against
a dropper, and compatible with the live read's `KEY SHARE`. The four lock modes are
named rather than passed as a boolean, since the difference between them is the whole
argument. Measured on a live server: `KEY SHARE` against `FOR UPDATE` blocks, `KEY
SHARE` against `FOR NO KEY UPDATE` does not.

The same drop has a second window. Cursors cascade from the namespace row, so a drop
landing before the fetch resolves its cursor deletes that row first, and an absent
cursor is indistinguishable from an expired one: the stream would close as
`ErrListenAged` and advise re-anchoring, which cannot succeed against a namespace
that no longer exists. On the unlocked path an expired cursor now rechecks the
namespace generation, and a namespace that is gone or replaced reports the lifetime
cause instead. Both windows exist only because the live read stopped taking the
namespace row's exclusive lock; the locked path serialized against the dropper.

The live pump no longer pays for work it does not need. It reads through a
transaction that does not take the namespace row's exclusive lock, so it stops
contending with the writer it is trying to keep up with; it mints a page's cursors
in one statement rather than one per record, which turns a nine-thousand-record
replay from about nine thousand inserts into ninety-one; and it keeps fetching while
pages come back full instead of re-probing between them. Together these took the
measured peak queue depth in the overflow fixture from 699 to about 6500.

That measurement is also what closed the last conformance gap, and it showed the
earlier reading of that gap was wrong. `TestSubscribeOverflowTeachesReconnect` parks
the subscriber at its replay boundary and floods, expecting the queue to overflow and
the stream to close teaching a reconnect. It flooded nine batches of
`MaxChangesPageLimit` against a bound of eight, which is a margin of one batch. That
margin is free under SQLite, where the writer pushes into the queue as part of the
commit, so the queue holds everything the writer has written. It is not free under
PostgreSQL, where the pump reads committed rows and therefore trails the writer; at
nine batches it peaked around 6500 of 8000 and the stream simply delivered all nine
thousand records. The earlier conclusion — that a reader of committed rows cannot get
far enough ahead — had the mechanism backwards. The pump does not need to get ahead of
the writer at all; it needs to put more into a parked queue than the queue holds, and
the fixture was not asking for enough to make that certain whatever the lag.

The flood is now sized from the bound rather than written as a literal, at twice
`ListenQueueBound`, so overflow is reached however far the pump trails. That constant
was duplicated in both engines and is now exported from `internal/store`, which is
where the cross-engine contract the conformance suite pins belongs. Both engines pass
the fixture repeatedly, and PostgreSQL passes it faster than it used to fail it,
because the stream now terminates at the bound instead of delivering every record.

The conformance matrix has no remaining PostgreSQL failures, and CI now runs the whole
suite under `DOLMEN_ENGINE=postgres` rather than the `BackendConformance` subset alone.
Until now nothing stopped a PostgreSQL-only regression landing: the matrix was clean
only when someone ran it by hand.

The driver remains pure Go and compatible with the static binary requirement.
PostgreSQL dependency versions are pinned in go.mod. No PostgreSQL server is bundled
with dolmen and no service is started by opening a store.

## Changing the catalog

The catalog carries a version, but it is read when a process **opens** the catalog, not
per operation. A process already holding the catalog is never re-checked, so during a
rolling upgrade an older process keeps issuing its own queries against the new shape for
as long as it runs. That is the constraint every catalog change has to survive, and it
decides between the two available shapes:

**Widen in place** when the change cannot alter which rows an existing query matches.
Adding a nullable column under an unchanged key is the safe case: an older process's
query returns exactly the rows it did before, whatever it selects. `changes.owner` is
this shape — `PRIMARY KEY(namespace,position)` is untouched and `changes_owner_feed` is
a plain index, so nothing an older reader asks for can answer differently.

**Move to a new relation and drop the old one** when it can. Touching a primary key, a
unique constraint, or anything an older query's matching depends on changes results
under a running process rather than failing in front of it. The idempotency record is
this shape: widening its key from `(namespace,table_name,drop_generation,key)` to
include `owner` took an older four-column lookup from matching at most one row to
matching one per owner, and `QueryRow` returns whichever comes first — another
principal's result, silently. Dropping the old relation makes that process fail instead,
which is the right answer for one reading a catalog it no longer understands.

A migration that leaves records without the new column needs the reading side to fail
closed on them rather than filter them away: a caller shown a page with its own rows
silently missing has been told something false. `ErrScopedFeedPredatesLabels` and the
idempotency legacy-owner domain are the two shapes of that, one refusing and one
admitting the unlabelled domain to callers who may read the whole table.

**Every PostgreSQL test builds its own catalog schema** (`testConfig` mints
`dolmen_test_<random>`), so the conformance suite only ever exercises a *fresh* catalog.
No migration path is covered by it. A catalog change is untested until a test shapes a
catalog backwards — creates the older relation, re-runs `bootstrap`, and asserts what
survived. `TestPostgresIdempotencyCatalogRetiresTheOwnerlessRelation` and
`TestPostgresChangeLogGainsItsOwnerColumnOnUpgrade` are the two worked examples.

A related trap outside the catalog, which this engine does **not** escape. A guard
belongs in a transaction, and that is not the same as belonging on the writer — but the
distinction that matters here is narrower still. `PlanMigration` runs under `s.read`,
which is not read-only: `readMode` takes `FOR SHARE` on the namespace row for every
caller except `readOnly`, and `FOR SHARE` conflicts with the `FOR NO KEY UPDATE` that
`s.write` takes. A dry run therefore holds every write in the namespace for as long as
its full-table counts run, exactly as it would on the writer. Reaching for a non-writing
transaction is not enough; it has to be a genuinely read-only one.

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
