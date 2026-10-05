# Changelog

## Unreleased

- Lakehouse slice 7 adds `read_rows` and typed reads: numbers, booleans, `json`, vectors, secrets
  (masked or revealed), timestamps and owner scopes read back exactly as on SQLite, which a
  both-engine conformance test pins; `query` results are typed by column label the same way.

- Lakehouse slice 11 adds `query` over `dolmen-duckdb`, a C++ sidecar on the pinned prebuilt
  DuckDB, one per namespace, over a framed stdin/stdout protocol. It is sealed read-only and
  confined to the namespace's data directory before caller SQL runs, sees each acknowledged append
  on the next query, binds arguments, pages and truncates like the other engines, and cancels on
  the deadline. The engine is still not selectable.
- Warn once when forwarding headers from a peer outside `DOLMEN_TRUSTED_PROXIES` are dropped, and
  once when a proxy's path-prefix hint cannot be used, instead of silently advertising a
  `base_url` without the prefix (#577).

- Lakehouse slice 6 adds internal appends. Row ids, change records, the idempotency record and
  exact per-owner row counts commit together in the namespace's SQLite commit log; each commit then
  lands in Iceberg as one fsynced Parquet file, replayed after a crash without duplicating rows.
  Lakehouse catalog format 3. The engine is still not selectable.

- Lakehouse slice 5 adds internal table lifecycle, versioned schema evolution, dry-run plans,
  migration history, and durable drop/recreate guards. Schema properties and native Iceberg
  evolution publish atomically; table lifecycle conformance runs on SQLite and lakehouse.
  SQLite now enforces the supplied DropTable incarnation before removing a successor table.

- Change-feed records now carry optional `commit` transaction grouping across polling, pagination and SSE. Legacy records remain unlabelled. SQLite catalog format 5 keeps minimum reader 3; PostgreSQL catalog 9 adds the column and namespace counter in place.

- Change `-shutdown-grace 0` to immediate cancellation and cap shutdown cleanup at five seconds, including stuck store close and stdio cleanup.

- Bound PostgreSQL SQL-compiler admission and caller waiting, check cancellation in SQL/filter compilation, and keep rollback cleanup within the caller deadline.

- Fix immediate `wait_for` polls on PostgreSQL: `timeout_ms: 0` skips polling while reading committed changes under the operation deadline.

### Added

- Lakehouse slice 4 adds internal namespace lifecycle with one SQLite-backed Iceberg catalog and
  separate Parquet data directory per namespace, durable namespace lifetimes, leaf-only drop,
  bounded handles, unpinned drop of unreadable catalogs or incomplete directories, listings that
  skip incomplete namespaces, and a SQLite/lakehouse lifecycle
  conformance test. Public lakehouse selection
  remains unavailable until its planned slice. Iceberg v0.6.0, Arrow v18.6.0 and Parquet v0.32.0
  are pinned with a version-pin test. Marc's 2026-10-03 order is recorded: 4 → 5 → 6 → 11,
  then 7–10 and 12–14.

## v0.5.0

### Added

- Spike 3 for the lakehouse query sidecar compared DuckDB with DataFusion, and DuckDB was chosen. The
  DuckDB sidecar and the Go driver that attacks it stay under `spike/`, outside dolmen's `go.mod`;
  the comparison is recorded in `docs/design/lakehouse-plan.md` §2.8.

- **`batch`: several writes in one transaction.** Send an ordered `writes` list against one namespace
  and they commit together or not at all, with one result per write in the order you sent them, one
  change-feed commit, and one `idempotency_key` covering the whole batch (a replay returns the stored
  results and says `replayed: true`). Each entry is one of `insert`, `update`, `delete`, `upsert` or
  `upsert_by_key` with that operation's own fields plus a `kind`; `namespace` and `idempotency_key` are set once per batch and refused inside a write, and
  `dry_run` is refused inside a write because a batch cannot mix a preview with writes that commit;
  `limit` and `confirm` are set once per batch for the same reason a single set would not do. At most 100 writes and 1,000 rows touched across the whole batch, so a batch is
  not a way around the per-call limits. An error names the failing write as `writes[i]` and keeps that
  write's own error class, and nothing is written when any write fails. Prefer one batch when the
  writes belong together and several smaller ones when they do not: a batch holds the server's single
  writer for its whole duration, `vectorize` provider round trips included.
- **`Store.Batch` on the Go facade.** The same operation, reachable from an embedder: an ordered
  `[]dolmen.BatchWrite` applied in one transaction, with `BatchOptions{IdempotencyKey, Limit, Confirm}`
  and one `BatchWriteResult` per write. It is also the only route to the filter-matched upsert, which
  has no single method on the facade, and `Limit` and `Confirm` are part of the hashed body as on the
  wire.
- **A measured comparison of the two `query` options for the lakehouse tier**
  (`docs/design/lakehouse-plan.md` §2.6–§2.7), which changes what Q5 asks. The in-process
  pure-Go engine, DoltHub's `go-mysql-server`, **is not confinable**: `LOAD_FILE()` and
  `SELECT ... INTO OUTFILE` read and write arbitrary files under `IsReadOnly` and
  `IsServerLocked`, no setting closes either, and a registered database is reachable across
  namespaces with `SHOW DATABASES` enumerating them all. It is genuinely pure Go and it costs
  **+61 MiB, +131%**, measured in CI. It lives in `spike/inprocess`, a nested module, so the main binary and
  `go.mod` are untouched. The DuckDB socket in the same section: the stock CLI has no listener, and
  the one extension claiming a server protocol is unpublished (HTTP 404 on every platform) and
  would be a silent no-op under the lockdown anyway — so slice 11 needs a wrapper dolmen builds and
  ships, or nothing. No engine code beyond what the spikes need.
- **A DuckDB lockdown spike** (`internal/duckdblockdown`), deciding whether a lakehouse `query` over
  a DuckDB process can be confined. A minimal helper starts a CLI locked to one namespace's data
  directory and the tests attack it: `ATTACH` to a sibling namespace, the namespace's own SQLite
  catalog, a symlink planted inside the allowed directory, `..` traversal, `COPY` in and out,
  `COPY ... TO PROGRAM`, the file readers, extension install and load, http(s) URLs, and every
  attempt to re-open the settings. **All are refused.** Two findings are recorded in
  `docs/design/lakehouse-plan.md` §2.4–§2.5: `enable_external_access=false` is the guard and
  `allowed_directories` *widens* what stays reachable rather than narrowing it, and the CLI runs
  `.shell`/`.system`/`.output` dot-commands from caller SQL under that full lockdown, in every
  mode — so a stdio transport would hand a caller code execution, and the transport is a unix
  socket instead. DuckDB is pinned to v1.5.6 and downloaded checksummed in a CI job; the tests skip
  without `DOLMEN_TEST_DUCKDB`. No engine code: nothing is wired into `store.Engine`.
- **`lakehouse` is a recognised engine name that is refused rather than unknown.** `DOLMEN_ENGINE`
  and `-engine` accept it, and the unknown-engine message now lists all three engines, so a
  configuration naming the lakehouse tier fails with a message that says what is and is not
  available instead of claiming the name does not exist. Every surface refuses it clearly until the
  engine is implemented: the Go facade (`WithEngine`) names the design document, the binary refuses
  at startup, and `dolmen backup` and `dolmen restore` refuse rather than quietly treating a
  lakehouse configuration as a SQLite data directory. **Behaviour change:** `-engine lakehouse`,
  `WithEngine("lakehouse")` and `DOLMEN_ENGINE=lakehouse` with `backup`/`restore` were previously
  unknown-engine errors or a silent SQLite fallback and are now teaching refusals; nothing that
  worked before changes. The lakehouse engine itself lands in later slices, per
  `docs/design/lakehouse-plan.md`.
- **A lane plan for the lakehouse engine** (`docs/design/lakehouse-plan.md`): the slice order for
  adapter #3, one mergeable PR at a time, following the `query` decision in
  `docs/design/query-without-sql.md` (an external DuckDB process below the seam, SQL passed through
  unchanged with its dialect disclosed, one engine process per namespace, and a pure-Go
  Parquet/Iceberg tier with the catalog in the namespace's own SQLite file). It covers the sidecar's
  start, supervision, resource limits and confinement, how the DuckDB side ships beside a
  `CGO_ENABLED=0` binary and what that does to `release.yml`, the append/position-delete/compaction
  write path and the namespace commit log the change feed needs, native engine-side full text and
  exact vector search, which parts of the shared conformance suite run against the engine and from
  which slice, the `arrow-go` v18.6.0 pin and what keeps it from breaking, and the questions that
  are Marc's. No code, no behaviour change.
- **Marc's answers to the lakehouse plan**, recorded in the same document: slice 3 ships next, the
  `query` lockdown is pulled forward into a spike that decides whether the sidecar path exists at
  all, and the `go.mod` pins land with the first code that imports them rather than as an inert
  module. On confinement, DuckDB's own settings are the only guard — no statement filter in dolmen,
  not even as a second layer — and if that is not enough the fallback is an in-process pure-Go SQL
  engine, then a declared `query` unavailability through the reserved spec amendment. Full text is
  native, settled by D27, with no spec amendment. Docs only.
- **An observability guide** (`docs/observability.md`): a runnable `docker compose` stack that
  receives dolmen's traces, metrics and logs, how to get from a slow `dolmen.operation.duration`
  bucket to the trace behind its exemplar, and three starter alerts.
- **Storage spans for plain reads and schema lifecycle calls.** `read_rows` records `SELECT <table>`
  and `query` a bare `SELECT` (arbitrary SQL may span tables, so it names none), while
  `create_table`/`drop_table` record `CREATE`/`DROP <table>` and `create_namespace`/`drop_namespace`
  `CREATE`/`DROP` with `db.namespace` only, on both engines. Read latency now shows its storage time
  in a trace; nothing new is recorded when tracing is off.
- **Engine capacity as OpenTelemetry gauges.** `dolmen.namespaces.open`, `dolmen.vector_cache.usage`
  and `dolmen.vector_cache.limit` on SQLite, and `db.client.connection.count` (split by
  `db.client.connection.state`, `idle` or `used`) with `db.client.connection.max` on PostgreSQL, are
  recorded as observable up-down counters whenever a meter provider is configured, so a pool or a
  vector cache heading for its limit is visible before requests start failing. The attributes stay
  bounded: no namespace, table or principal is ever named. `telemetry.Provider` now exposes its
  `MeterProvider`, the way it exposed its `TracerProvider`.
- **`shape` on `json` fields** (#128). A `json` field may declare `object`, `array`, `array<string>`,
  `array<number>`, `array<boolean>` or `array<object>`; every write path refuses a value of any other
  shape, naming the field, the expected shape and what arrived. `migrate` gains `set_shape`, which
  refuses a shape stored rows do not fit and needs `read` as well as `schema`.
- **OpenTelemetry metrics over OTLP.** Operation duration and outcome, operations in flight, active
  subscriptions, `http.server.request.duration`, and the `gen_ai.client.*` embedding metrics, pushed
  to the same collector as traces (`WithMeterProvider` in Go). `GET /metrics` still serves
  Prometheus. **Behaviour change:** `OTEL_EXPORTER_OTLP_ENDPOINT` now turns metrics on as well as
  traces; set `OTEL_METRICS_EXPORTER=none` to keep traces only.
- **`search_fulltext` scores every hit as `_score`**, higher being more relevant, like `search_vector`.
  The scale is the engine's own (FTS5 BM25 negated on SQLite, `ts_rank_cd` on PostgreSQL), so compare
  scores only within one query's results.
- **Log export over OTLP.** `OTEL_LOGS_EXPORTER=otlp` also sends every log line to the collector,
  honouring `-log-level` and carrying the request's trace context; stderr is unchanged. It is off
  unless set, even with an endpoint configured.
- **`describe_table` reads a kept row count instead of scanning the table.** Every write keeps
  a per-table count current (per owner on `row_access` tables), so `row_count` costs the same on a
  table of any size. Existing namespaces are counted once on first open. On SQLite this raises the
  namespace's catalog minimum-reader stamp to 3, so an older dolmen refuses the directory afterwards;
  on PostgreSQL the catalog moves to version 7. Back up before the first open if you may need to
  downgrade.
- **`secret` field type.** Values are encrypted at rest with AES-256-GCM under
  `DOLMEN_SECRET_KEY` / `DOLMEN_SECRET_KEY_FILE` (or `WithSecretKey` in Go), read back as `"••••"`
  on every path, and are returned in plaintext only when named in `reveal` on `read_rows` or a
  search. Under `-auth on` reveal needs the new `reveal` verb, which `admin` does not imply, and
  every reveal writes an audit log line without the value. Both engines support it (PostgreSQL
  stores the ciphertext in a `bytea` column). See `docs/design/secret-fields.md`.
- **Secret key rotation.** `DOLMEN_SECRET_KEYS_OLD` / `DOLMEN_SECRET_KEYS_OLD_FILE` (or retired
  keys passed to `WithSecretKey`) keep old keys for decryption, and the new `rotate_secret_key`
  operation (`admin` on `*`; `RotateSecretKey` in Go) re-encrypts stored values under the active
  key in bounded, resumable batches, reporting values per key id so you know when a retired key can
  go. Idempotent inserts replay across a rotation. See the README runbook.
- **SSE frames carry their cursor as `id:`, and `Last-Event-ID` resumes a stream.** `change`, `ready`
  and `close` frames on `/v1/subscribe` now carry the SSE `id:` field, and a `Last-Event-ID` request
  header is the resume cursor, taking precedence over `cursor` — so a browser `EventSource` that
  reconnects resumes where it left off instead of at its original cursor. `Last-Event-ID` is allowed
  in CORS preflights (#552).

### Changed

- **A vectorizing `migrate` is resumable, on both engines.** The embedding backfill used to run
  inside the write transaction, so a provider that died, a `504` timeout, or a killed process threw
  away every vector it had produced and the next attempt started from the first row; the embeddings
  were also written where a reader could see them before the migration landed. Each batch is now
  embedded outside the write transaction and kept on disk, keyed by table, drop generation, provider
  identity and the SHA-256 of the row's text, and activation fills the column from it in one short
  transaction. Re-issuing the same `migrate` embeds only the rows it does not already hold. A staged
  vector is never reused across a provider identity or a model change, a row whose text changed after
  it was staged is embedded again rather than stamped with a stale vector, and staged rows are dropped
  when the migration lands, when the table is dropped, and on `vacuum`. The plan a `dry_run` returns
  now carries `staged_rows` beside `embed_rows` — the rows still needing a provider call and the ones
  an interrupted attempt already finished, which add up — and the `504` and `409` messages for a
  stopped or out-repeated migration say the progress is kept. A data directory written by v0.3.0 or
  earlier is adopted as before: the SQLite catalog format moves to 4 and the PostgreSQL catalog to 8,
  and a format-3 reader still sees a table as it was before activation. **Behaviour change:** on
  PostgreSQL, a `migrate` that would vectorize now refuses on a database whose `server_encoding` is
  not UTF-8, naming the encoding it found and the `CREATE DATABASE … ENCODING 'UTF8'` that fixes it,
  because the staged digest compares bytes and nothing would match; other migrations still run there.
- **The PostgreSQL connection pool defaults to at most 20 connections.** pgx sized an unconfigured
  pool at one connection per CPU, which on a large host exceeds PostgreSQL's default
  `max_connections`, so one busy server could hold every slot and starve another sharing the
  database. Set `pool_max_conns=N` in the DSN to raise it (#559).
- **A full-text `query` is limited to 2,048 bytes** on both engines and the Go library, and refused
  with `invalid_request` before any work (#557).
- **Skill corrections.** Both skills document querying `json` fields with `::jsonb` (and why the `?`
  operator cannot be used), list `batch` with a worked example, state that PostgreSQL full-text does
  not fold accents or segment CJK and emoji, say what `row_access` does not hide (shared row ids, and
  the total `row_count` without `row_access`), and correct several smaller points the pre-release
  black-box rounds tripped on (#548, #555, #563).

### Fixed

- **An operation past `-op-timeout` is now answered at its limit on PostgreSQL.** The limit bounded the
  engine's statement but not dolmen's own work, so the first query carrying a user-written filter or
  `query` body in a process was charged the confined-SQL parser's one-time start-up:
  `wasilibs/go-pgquery` compiles its WASM module on the first `Parse` and parses in microseconds
  afterwards, and the compile ran on the caller's goroutine without a context. A 300 ms limit could
  therefore be answered about two seconds late, and far later on a loaded or small runner — the
  connection stays busy for the whole delay, and a client built around the limit hangs. The parser
  now starts warming when the store opens, so the cost lands on start-up instead of on a caller's
  operation, and compiling a query or a filter waits for that start-up under the caller's own
  context and reports the deadline rather than running through it. A cancelled or expired caller is
  still reported as cancelled or timed out at the limit. On SQLite, whose driver stops the statement
  at the deadline, nothing changes. The write semantics are untouched: a write past its limit still
  says it "may or may not have committed".
- **`describe_server` says what the next vectorized write will meet, instead of a boolean that reads
  as a fault.** `model_cached: false` on a server that has never served a vectorized write is an
  ordinary first run — the model is a Hugging Face model that first use downloads — but in
  isolation it reads as "the embedding model is broken or missing". A cold-start trial with no
  network took it as the service's largest risk and planned around a failure that did not happen,
  and the very next call downloaded the model and succeeded. The boolean also conflated two states
  that need opposite responses: a model that will download itself on first use, and a
  `DOLMEN_EMBED_MODEL` directory that is incomplete and that no download repairs. The field is now
  `model_state`, with three values a caller can branch on without reading prose: `cached` (the
  weights are complete, nothing is downloaded), `download_on_first_use` (a normal cold start, which
  can take ten seconds or more and can fail transiently — retry, or pre-seed the cache), and
  `incomplete` (an operator has to fix or replace the directory). The value is declared as an enum
  in the published output schema, so a caller reading the schema sees the three states without
  reading the skill, and the README and both skill documents describe the same three. The field is
  present for the `local` provider exactly where `model_cached` was, and absent for `none` and
  `openai`. `usable` is untouched: it still answers "will a vectorized write work", the question it
  was added for, and it is still true in the `download_on_first_use` state. A caller that read
  `model_cached` should read `model_state` instead: `model_cached: true` is `model_state:
  "cached"`, and its `false` is now one of the two other states rather than one undifferentiated
  one. The startup warning picks its message from the same value, so the log and the operation
  cannot disagree about which case applies.
- **`wait_for` answers a page when a read runs out of its budget, even on the first attempt.** A read
  that overran the loop's own budget was supposed to answer the documented empty page, but that only
  happened once one read had already succeeded: the first attempt's failure was returned as an
  error, so a `timeout_ms: 0` conditional poll resuming from a cursor — a read slow enough to outrun
  the budget, which PostgreSQL does under load — came back `504 timeout` with the generic "the
  server ran out of time" message instead of a page. An agent polling a cursor could not tell
  "nothing has changed yet" from "the server gave up", and the advice in that message — check with a
  query before retrying — is a workaround for a wait that was meant to be free. A wait that was given
  a cursor now answers the empty page carrying that same cursor whenever the caller's own context is
  still live, which is what the operation description, the README and the skill have always
  promised. A wait that passed no cursor still fails when its first read outruns the budget, because
  there is no boundary to hand back and an empty `next_cursor` cannot be resumed from; a cursor the
  feed rejects is still an error, and so is a call whose own context ended. SQLite hid this because
  its driver does not notice the budget, so the same call answered a page on that engine.
- **Request field names are matched exactly, and a `null` body is no longer an empty one.** A key
  that differs only in case is an unknown field, not a type mismatch on a field the advertised schema
  does not contain: `{"Namespace":"x"}` used to work, and `{"sql":1,"bogus":2}` and
  `{"bogus":2,"sql":1}` got different error classes purely from key order — an unknown field now
  wins, in either order, at every level of the body. `null` is refused like any other non-object
  body (`400 invalid_request`), as MCP already refused it; only a body with nothing in it is `{}`.
  MCP still answers a non-object `arguments` member with JSON-RPC `-32602`, since the MCP
  specification calls invalid tool arguments a protocol error; that intended difference is recorded
  in the facade input matrix. **Behaviour change:** a miscased key that used to be accepted is now an
  error. Row keys are unaffected: a record is still checked against the table's own schema.
- **A namespace whose file cannot be read is refused, not silently re-initialized.** Reopening a
  data directory whose namespace file was truncated to nothing used to let the next write through:
  SQLite reads a zero-length file as an empty database, so `create_table` succeeded and the rows
  written before the truncation were gone with no error anywhere. Every operation on a namespace the
  startup scan found unreadable now refuses before opening the file, naming the namespace, the reason
  it could not be read and the remedy — restore it from a backup, or `drop_namespace` it, which still
  works on an unreadable file. A corrupt catalog stamp keeps its own `400 invalid_request` and its
  message naming the bad value, and a namespace written before the catalog gate is still adopted
  rather than refused. Other namespaces, discovery and the health probe are unaffected, and a
  namespace serves again as soon as its file is restored, with no restart.
- **A cancelled request is answered as `canceled`, not as a server fault.** A request whose context
  was done used to be answered with whatever the failure classified as, so a failure the caller
  could not have acted on — an engine reporting an interrupted statement in its own words
  (`interrupted (9)`, which dolmen does not recognise), or one of dolmen's own refusals raised while
  the caller was already leaving — reached the wire as `500 internal_error`, and the Go library as
  `ErrInternal`. The caller's context now decides the class at both boundaries, over `/v1`, over MCP
  and in the Go library, for every operation and every read path: cancelled is `canceled`, an expired
  deadline keeps its own `timeout`, and a live caller's fault is still reported as the fault it is.
  The engine's own error is kept as the cause, so it still reaches the log.
- **`query` can no longer hand out a secret's ciphertext.** Masking used to key off the result-column
  label, so `SELECT token AS t` returned base64 of the stored bytes and `length(token)` its unpadded
  length — any caller holding `read` could exfiltrate every secret's ciphertext without the `reveal`
  verb. Every reference to a table holding a secret is now rewritten to project the mask in the
  column's place, so aliases, expressions, subqueries, CTEs, `SELECT *` and `ORDER BY` all read
  `"••••"`. Search and write filters still evaluate against the ciphertext, where they select rows
  but return no values.
- **Writing a masked secret back no longer destroys it.** `"••••"` is refused as a `secret`
  value, naming the field and saying to pass the real value, `reveal` it first, or omit the field.
  An agent that read a row without `reveal` and wrote it back edited previously stored the mask as
  the plaintext, losing the secret silently and unrecoverably.
- **A huge full-text query no longer wedges its namespace.** On PostgreSQL a multi-megabyte
  `search_fulltext` query kept every write to its namespace waiting for minutes: the query compiler
  was quadratic in the number of terms and ran inside the read transaction holding the namespace
  lock, ignoring cancellation and `-op-timeout`. The compiler is now linear and oversized queries are
  refused up front (#557).
- **A NUL character (`\u0000`) anywhere in a request is refused with `invalid_request` naming the
  field.** It used to answer `500` in a table name, cursor, idempotency key or `tokenize` text, an
  opaque `query_error` in a SQL argument, and inside a `json` value it was stored, after which every
  `::jsonb` query on that table failed until the row was deleted. String, text and secret values are
  refused at coercion too, so the Go library agrees (#536, #561).
- **A PostgreSQL write that races a `migrate` retries instead of failing with a version conflict**
  the caller never asked for (#544).
- **PostgreSQL connection exhaustion answers a retryable `timeout`** saying the database is at its
  connection limit (or, for SQLSTATE 57P03, restarting), instead of a bare `internal_error`; nothing
  was started, so a retry is safe (#550).
- **A too-complex PostgreSQL statement (SQLSTATE class 54) is `invalid_request`** telling you to
  simplify it, not an opaque `query_error` (#546).
- **`batch` errors.** A missing, empty or malformed `namespace` is `invalid_request`, not a `500`; a
  SQL or filter error inside a write keeps its `writes[N]:` prefix on both engines; a write that is not
  an object says so instead of leaking a Go type name; and the rows-touched budget message describes
  the per-batch budget and the way through for a large delete (#536, #540, #542, #563).
- **`search_vector` refuses an all-zero query vector and whitespace-only `text`**, both of which used
  to return arbitrary rows (#538).

## v0.4.0

### Added

- **Idempotency keys are namespaced by owner.** A key is unique per table *and* writer principal,
  so two principals using the same string are using two different keys — neither conflicts, neither
  reveals the other, and the first to use a key cannot squat it. A retry consults only its own
  domain, so it finds its own record through grant changes, gained table-wide `read`, and
  `row_access` disablement alike; the payload comparison that rejects a changed body therefore never
  runs against someone else's record. Records written before `-auth on` are preserved and replay to
  callers holding table-wide `read`, whose rows those ids already are; turning auth off again
  consults only that pre-auth domain, so `auth: off` behaves exactly as it did in v0.2.0. This lifts
  the refusal of `idempotency_key` for a caller restricted to their own rows.

  **This one is a one-way door.** Existing databases gain the new column on first open, and the
  namespace's catalog minimum-reader stamp rises with it, so an older dolmen refuses to open the data
  directory afterwards rather than opening it and reading the table as though keys were still global
  — which is precisely the disclosure being closed, since that reader would answer one principal's
  retry with another's ids. The records also move to a new table rather than growing a column in
  place, so an older dolmen that already had the directory open when the upgrade ran fails loudly on
  its next idempotent insert instead of quietly reading the new layout with its ownerless lookup: the
  catalog stamp can only refuse the *next* open, not a process already running. Back up a data
  directory before the first open if you may need to downgrade.

  On the PostgreSQL engine a scoped caller's `idempotency_key` is still refused: the owner-keyed
  record is an adapter #1 change, and adapter #2 keeps failing closed until it has one.

- **Filters are a row-local language when auth is on.** `update`, `delete`, `upsert`,
  `search_fulltext` and `search_vector` take a SQL `WHERE` fragment; with `-auth on` that fragment
  is now parsed and checked against a fixed allowlist before it reaches the engine — the target
  table's own columns, literals, `?` arguments, and an enumerated set of operators and functions.
  Subqueries, references to another table, aggregates, window functions and anything else are
  `invalid_request`. Two reasons: a grant on one table must not reach the table next door through
  a subquery, and under a row scope a filter that can read or raise on an invisible row is an
  oracle over it. Date and time functions take explicit moments only — `'now'`, `'localtime'` and
  `'utc'` are refused whether written as literals or bound as arguments; compute the moment you
  mean and bind it. Under `auth: off` the filter language is exactly what it was in v0.2.0.
- **Scoped callers can filter again.** `update`, `delete`, `upsert` and filtered searches were
  refused outright for a caller limited to their own rows; they now work, with the scope applied
  first at a materialization boundary so a caller's expression never evaluates against a row
  outside their visible set. The allowlist alone cannot deliver that: `iif(body = ?,
  abs(-9223372036854775808), 1)` is row-local and fully permitted, and as an ordinary `AND`
  conjunct it still raises an overflow error on a foreign row — which answers what that row
  holds. An upsert whose filter matches only invisible rows inserts a fresh row owned by the
  writer rather than taking one over.

- **`-auth on` / `DOLMEN_AUTH=on`** turns on deny-by-default authentication, with the bootstrap
  admin key (`DOLMEN_ADMIN_KEY`) as its one identity source. Every `/v1/{op}`, `/mcp`, and
  `/v1/subscribe` request without an accepted credential answers `401` with the new `unauthorized`
  error code; rejections are uniform, so the message never says which part failed. `/healthz`,
  `/version`, `/skills*`, and `/v1/openapi.json` stay unauthenticated in both modes. This is a
  shared credential for one administrative principal, not a multi-user system — per-user
  identities, API keys, and permissions are the following slices. `dolmen mcp` refuses to start
  with auth on: a stdio pipe carries no per-request credential.
- **Identity asserted by a gateway.** `-trusted-proxies` / `DOLMEN_TRUSTED_PROXIES` lets peers
  inside the given CIDRs assert `X-Dolmen-Principal` and `X-Dolmen-Groups`. Trust is decided from
  the immediate TCP peer, never from `X-Forwarded-For`; headers from other peers are ignored
  entirely. `-max-groups` / `DOLMEN_MAX_GROUPS` (default 128, range 1–1024) caps the group list,
  and an over-limit list fails the identity rather than dropping a group that might carry a grant.
  An asserted identity authenticates and is then refused `403 forbidden` until the grant ops land —
  deny-by-default, which is what makes the source safe to enable before permissions exist.
- **Grants.** `grant`, `revoke`, and `list_grants` give a principal or group verbs — `create`,
  `read`, `update`, `delete`, `schema`, `admin` — on a namespace, a table, or `*`. Grants inherit
  downward and combine by union, with no deny grants and no precedence. The verb gate covers every
  operation, `/mcp`, and `/v1/subscribe`. `whoami` reports the caller's identity so an agent can
  self-diagnose a `403`. All four ops exist only when auth is on, and are absent from dispatch,
  `tools/list`, and `openapi.json` when it is off.
  Authorization precedes existence: an ungranted caller gets `403` whether or not the object
  exists, `list_tables` answers `404` for a namespace they hold nothing under, and
  `list_namespaces` returns only what they can reach. Implicit namespace creation is disabled when
  auth is on, since it would bypass the parent's `admin` gate. Dropping a namespace or table
  removes the grants targeting it, and revoking the last root administrator is refused while no
  replacement exists.
- **Per-row ownership.** A table declared with `row_access: "own"` carries an implicit `owner`
  column the server stamps on every insert; callers never supply it. A caller holding `read` sees
  the whole table, a caller holding only a data verb (`create`/`update`/`delete`) sees the rows
  they wrote, and a `schema`/`admin`-only holder sees none — `describe_table` reports 0 rather than
  the real count. The annotation exists only when auth is on, and never appears in a schema when it
  is off. Enabling it later through `migrate set_row_access` is refused on a table that already has
  rows, because no operation can write another principal's rows as that principal; turning it off
  keeps the column and its values and requires `admin` as well as `schema` and `read`. Realtime feeds follow the
  same rule: a caller holding only a data verb on a `row_access` table may now subscribe and
  receives its own rows, where it used to be refused outright, and every change record carries the
  owner of the row it describes so the live feed can tell them apart. A scoped subscription that
  would replay history recorded before those labels existed is refused rather than served with the
  unlabelled records silently missing; starting at the current head is always available.
  `changes_since` and `wait_for` follow the same rule: a scoped caller may name a table and catch up
  on its own rows, foreign commits never wake it, and the namespace-wide feed still needs `read` on
  the namespace because it reports every table. Cursor tokens are opaque random values, so a scoped
  reader's sequence cannot be told from one where the foreign commits never happened, and retention
  expires by age rather than by volume, so other principals' traffic cannot evict a quiet reader's
  cursor. A migration plan's
  `backfill_rows`, `fulltext_reindex_rows` and `embed_rows` now count only the rows the caller can
  see — a schema-only holder sees zeroes — while the rules they feed still run against every row,
  so redacting a count never redacts a constraint. A `set_enum` that stored values block is refused
  the same way for everyone, but only a table-wide reader is told which values and how many rows. The plan and its apply each verify the
  incarnation they were resolved against inside their own transaction. A dry run now returns an
  opaque `expected_incarnation` naming the table it planned against; pass it back on apply and a
  table dropped and recreated in between is refused rather than migrated under a stale plan. With
  authentication on, a precondition must use that token — `expected_version` alone is refused,
  because version 1 cannot tell a table from a same-named predecessor. With authentication off,
  `expected_version` keeps working unchanged. Scoped
  `upsert_by_key` matches the natural key inside the visible set: a key held by an invisible row
  counts as no match, so the insert branch runs and the two rows coexist under one key, and no id,
  count, or error tells the two situations apart. Scoped `update`, `delete`, `upsert`, filtered
  searches and idempotent inserts work the same way — see the filter language, the materialization
  boundary, and the owner-keyed idempotency entries above.
- **API keys.** `create_key` / `list_keys` / `revoke_key` mint credentials for machines that cannot
  do an interactive sign-in. A key authenticates as a principal and carries optional groups, but
  grants nothing by itself. The credential is shown once and stored hashed; keys are revoked by a
  server-generated id, so two sharing a name stay individually revocable; `list_keys` never returns
  credentials; and a revoked key is refused with the same `401` as an unknown one. Downstream of
  the seam a key identity is indistinguishable from a gateway-asserted one — same verbs, same
  grants, same row scope.
- **Native sign-in through an identity provider.** With `DOLMEN_AUTH_OIDC_ISSUER` (or
  `DOLMEN_AUTH_OIDC_PRESET=github`) set, `/v1/auth/begin` runs the authorization-code flow with
  PKCE and single-use state, and hands back an Ed25519-signed, stateless token. No user records, no
  session store. Principals and groups are issuer-qualified (`oidc:v1:<digest>:<claim>`) because a
  subject is unique only within its provider, so changing issuers mints a disjoint population
  rather than silently reassigning grants. The signing key and the deployment's token issuer id
  persist beside the grants, so tokens survive restarts and verify across replicas, and a
  deployment will not accept a token minted by another that happens to share its keyring.
  `rotate_signing_key` mints a successor; with `retire_previous` it stops honouring every token
  signed by an earlier key, which is how a deployment signs all its people out at once — immediately on the replica that
  served it, and within the keyring refresh interval elsewhere.
- **`unauthorized` error code** (401) in the shared taxonomy. `auth: off` never emits it, and the
  default is still `auth: off` — nothing changes for existing deployments.

### Changed

- **Embedding model tarballs are no longer attached to each release.** They are published once
  under their own tag (`models-v1`) and shared by every dolmen version, which keeps ~370 MB of
  identical bytes off every release. Existing download URLs for v0.3.0 and earlier keep working;
  new ones drop the version from the asset name and take the model tag instead, e.g.
  `releases/download/models-v1/dolmen-model-all-MiniLM-L6-v2.tar.gz`. Each release's notes link the
  model tag it was built against.

## v0.3.0

Realtime change feeds, an MCP stdio transport, and an in-process Go library. Five behavior
changes need attention before upgrading.

### Breaking

- **Reads no longer create namespaces.** `list_tables`, `query`, and `changes_since` used to
  answer `200` for a namespace that did not exist, creating its database file as a side effect;
  a mistyped name left `<ns>.db`, `-wal` and `-shm` behind and held their connections. Every read
  now answers `404 not_found` and creates nothing. The write ops (`create_table`, `insert`,
  `update`, `upsert`, `upsert_by_key`, `delete`, `migrate`) still create implicitly on first use.
  If you relied on a read to provision a namespace, call `create_namespace` or a write op instead.
- **The executable moved to `cmd/dolmen`.** Install with
  `go install github.com/lsm/dolmen/cmd/dolmen@v0.3.0`. Released binaries and the container image
  are unaffected.
- **`subscribe` on a missing namespace** is an in-stream `not_found` error instead of a silent
  `200` with an empty stream.
- **`describe_server.usable`** no longer reports `true` when the local embedding model is not yet
  cached. A new `model_cached` field reports the cache state separately. Clients that branched on
  `usable` to decide whether embedding would succeed now get an honest answer, and a different one.
- **Error messages changed** across the decode, idempotency, full-text, and embedding paths. All
  codes and HTTP statuses are unchanged. Only clients matching on message text are affected.

### Added

- **Change feeds.** `changes_since` replays a namespace's durable change log from a cursor;
  `wait_for` long-polls it server-side (default 30s, max 60s) so an agent blocks instead of
  spending tokens on a poll loop; `subscribe` (SSE, `GET /v1/subscribe`) streams replay-then-live
  with keepalive frames and a resume cursor.
- **`read_rows`** fetches rows by id, and **`capabilities`** reports the engine's static surface.
  Twenty-three operations total, up from nineteen, and none were removed. `openapi.json` lists 24
  paths: one `POST /v1/{op}` per operation, plus `GET /v1/subscribe`, which is a stream rather than
  an operation and so has no entry in `tools/list`.
- **MCP over stdio.** `dolmen mcp` serves the same dispatcher on stdin/stdout for hosts that launch
  a subprocess, with a bounded drain on shutdown.
- **Embedded Go library.** `import "github.com/lsm/dolmen"` opens a store in-process with no port
  or subprocess. The embedding provider is optional; `dolmen.Open(dir)` serves every non-vector
  operation.
- **Enum fields** restrict a string field to a closed vocabulary, evolved with `migrate set_enum`.
- **`now()` timestamp defaults** stamped server-side, so idempotent retries can omit the field.
- **Catalog format version** stamped per namespace. The server refuses to start against a data
  directory written by a newer dolmen, naming both versions. Downgrading to v0.2.0 is still safe:
  it ignores the stamp.
- **Sub-path hosting behind a stripping proxy.** The public prefix is recovered from a forwarded
  original URI (nginx `$request_uri` via `X-Original-URI`, plus the Envoy and ingress spellings),
  RFC 7239 `Forwarded` is honored, and the server warns once when it would advertise a base URL no
  proxied client can reach.
- **`-engine` / `DOLMEN_ENGINE`** selects the storage engine. Only `sqlite` is implemented; any
  other value is rejected at startup. This is the seam for future engines, not a preview of one.

### Security

- **A forwarded-header value could inject arbitrary text into the served skill markdown.** The
  sub-path inference added in this release accepted any single-line header value as the public
  prefix, and the skills interpolate that prefix into shell snippets an agent is told to paste and
  run. An unauthenticated request could therefore place attacker-chosen text, including quote-
  breaking shell characters and injected instructions, into the document another agent executes,
  and into the MCP `initialize` instructions. The same path amplified a 1 MB header into a 15 MB
  response. Prefixes are now validated (a conservative character set, no dot or empty segments, and
  length and segment caps), the skills single-quote interpolated URLs, and the skills and OpenAPI
  responses are marked `private, no-store` with a `Vary` so a shared cache cannot serve one
  request's rendering to everyone. The forwarded host and scheme are validated the same way, since
  they reach the same snippets and the OpenAPI `servers` URL, and the PowerShell snippet uses a
  literal-quoted string so it cannot interpolate either. Setting `DOLMEN_BASE_URL` was, and remains,
  a complete mitigation.
- **A JSON number that did not fit its field echoed the caller's literal back in the error.** A
  multi-megabyte literal produced a multi-megabyte error message. The message now names the type
  only.

### Fixed

- A revoked subscription could report a clean end instead of its revocation cause under race
  scheduling.
- A wrongly typed JSON value reported `invalid JSON` and leaked Go-internal type names; it now
  names the field and both types.
- Zero-argument `/v1` operations accept an empty request body instead of answering `415`.
- Full-text errors for bare hyphenated terms teach the quoting fix instead of blaming a missing
  column.
- Idempotency conflicts teach a remediation that does not create duplicates.
- User input can no longer rewrite an error message through the redaction gates.
- A namespace whose catalog is too new is refused before anything is written to it; previously the
  registry tables were recreated inside the newer database before the refusal.
- The Go facade rejects a `nil` record in `Insert`, as the HTTP and MCP surfaces already did.
  Previously it inserted a phantom row that bypassed required-field validation.
- `model_cached` reports `true` once the local model is on disk. The completeness check demanded a
  `config.json` for parameterless modules that are never materialized, so the default model reported
  itself uncached forever and every start logged a spurious download warning.
- `/v1/subscribe` appears in `/v1/openapi.json`, so a client working from that document can discover
  the live stream.
- A read against a missing namespace names the remediation instead of only the namespace.

### Upgrading

Point the new binary at the same data directory. No migration runs, and the catalog stamp is
written on first open of each namespace. Review the read-creates-namespace change first — it is
the one most likely to be load-bearing in existing client code.
