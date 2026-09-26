---
name: dolmen
description: End-user skill for Dolmen — query, insert, full-text/vector search, describe, list, and delete records against existing tables. Never invent schema; surface the need.
---

# Dolmen — durable agent data

A Dolmen server exposes tools over MCP. Everything lives in namespaces (isolated databases).

{{ .NamespaceHint }}

> This is the `dolmen` (core) skill. It is for reading and writing data in tables that already exist. If you may need to design, create, or migrate tables, use the `dolmen-admin` skill instead ({{ .BaseURL }}/skills/dolmen-admin).

## Setup

The running server is at `{{ .BaseURL }}` and the MCP endpoint is `{{ .MCPURL }}`. This skill matches server version `{{ .Version }}`.

The API's machine-readable description — every operation's request schema, the response envelope, and the error codes — is served at `GET {{ .BaseURL }}/v1/openapi.json`.

### Health check

Bash:

```bash
base='{{ .BaseURL }}'
curl -s "${base%/}/healthz"
```

Windows PowerShell:

```powershell
$base = '{{ .BaseURL }}'
curl.exe -s "$($base.TrimEnd('/'))/healthz"
```

Should return `{"status":"ok"}`. If the server is not running, do not improvise — start it first.

### Connect the skill

Bash:

```bash
claude mcp add --transport http dolmen '{{ .MCPURL }}'
```

Windows PowerShell:

```powershell
claude mcp add --transport http dolmen '{{ .MCPURL }}'
```

The `dolmen` tools then appear in `tools/list` with full input schemas. The endpoint can also be
read from the environment: `DOLMEN_URL` (default `{{ .BaseURL }}`).

Dolmen also ships a stdio transport — `dolmen mcp` (same flags) speaks the identical
JSON-RPC surface on stdin/stdout for hosts that launch the server as a subprocess; this
skill is rendered for an HTTP deployment, so prefer the connection command above when it
is reachable.

If the `dolmen` MCP tools are not connected, do not improvise — ask the user to re-run the
connection command above. MCP servers cannot be hot-loaded into an already-running session; when
no user is available to re-run it, use the JSON-RPC fallback below instead.

## When the server requires a credential

A server running with authentication on answers any operation that arrives without an accepted
credential with `401` and error code `unauthorized`. `/healthz`, `/version`, these skills and
`/v1/openapi.json` stay open, so reaching them says nothing about access.

Send the credential with every request, `/mcp` and `/v1/subscribe` included, as a bearer token:

```bash
base='{{ .BaseURL }}'
curl -s -X POST "${base%/}/v1/whoami" \
  -H "Authorization: Bearer $DOLMEN_TOKEN" \
  -H 'Content-Type: application/json' -d '{}'
```

```bash
claude mcp add --transport http dolmen '{{ .MCPURL }}' --header "Authorization: Bearer $DOLMEN_TOKEN"
```

The people who run the server issue credentials: an API key minted for you (it starts with
`dlm_`), or a token a person receives by signing in at `{{ .BaseURL }}/v1/auth/begin` in a browser,
on servers that offer sign-in. You cannot complete that sign-in yourself. If you hold no
credential, ask the user for one, and never put one in a URL, a filter, or a record.

`unauthorized` and `forbidden` call for different responses:

- `unauthorized` (401): no credential this server accepts. A missing, malformed, expired, or
  revoked credential all get the same message, so retrying with the same one cannot help.
- `forbidden` (403): you are authenticated, but no grant covers this operation on this object. Do
  not retry, and do not probe other tables for one that works. Call `whoami`, which needs no
  grant, and tell the user its `principal` and `groups` along with the operation and table you
  need, so an administrator can grant it.

A grant gives verbs on a namespace (covering its tables and sub-namespaces), on one table, or on
the whole server. What each operation needs:

| Operation | Needs |
|---|---|
| `read_rows`, `search_fulltext`, `search_vector`, `changes_since`, `wait_for`, `/v1/subscribe` | `read`, or on a table with `row_access` any of `create`, `update`, `delete` for your own rows |
| `insert` | `create` |
| `update` | `update` |
| `delete` | `delete` |
| `upsert`, `upsert_by_key` | `create` and `update` |
| `query` | `read` on the whole namespace, since SQL can reach any table in it |
| `describe_table` | any verb on the table |
| `whoami` (exists only when authentication is on), `list_namespaces`, `list_tables`, `describe_server`, `capabilities`, `infer_schema` | nothing |

A feed without `table` (namespace-wide `changes_since`, `wait_for` or `/v1/subscribe`) needs
`read` on the namespace itself; a grant on one table covers only that table's filtered feed.

Absence is not proof: `list_namespaces` lists only what you can reach, and `list_tables` answers
`not_found` for a namespace you hold nothing under. A write to a namespace that does not exist
answers `not_found` rather than creating it.

**Tables with `row_access: "own"`.** The server records who wrote each row. Unless you hold
`read`, you see and change only your own rows: reads, searches, filters, `update`, `delete`,
`upsert_by_key` and the change feed all run over them alone, and a row you cannot see never shows
up in a result, a count, or an error. Never send an `owner` field; the server stamps it, and
supplying it is refused like any unknown field. Idempotency keys are yours alone, so the same
string used by someone else is a different key.

With authentication off, a `filter` is a plain SQL WHERE expression in the server's dialect
(`filter_dialect` in `capabilities`). **Filters under authentication** are checked against a fixed allowlist, in SQLite's semantics on every engine: comparison, arithmetic,
`||` and boolean operators, `LIKE`, `IN`, `BETWEEN`, `IS`, `CASE`, and the functions `abs`,
`round`, `length`, `lower`, `upper`, `substr`, `trim`, `ltrim`, `rtrim`, `replace`, `instr`,
`coalesce`, `ifnull`, `nullif`, `iif`, `date`, `time`, `datetime`, `julianday`, `strftime`. A filter
sees only the row it is applied to, so no subqueries and no other tables, and nothing in it may
read the clock: `'now'`, `CURRENT_TIMESTAMP`, `localtime` and `utc` are refused. Compute the moment
you mean and bind it as a `?` argument. The error names whatever it refused.

## Raw HTTP

Every tool in this skill is also a plain HTTP operation: `POST /v1/{operation}` with the tool's input
as the JSON body (`Content-Type: application/json`). An empty request body counts as `{}`, so an
operation whose input fields are all optional (for example `list_namespaces`) can be called with no
body at all. Responses are enveloped — success is
`{"ok":true,"data":...}` and failure is `{"ok":false,"error":{"code","message","request_id"}}`
with a stable machine-readable `code` (`invalid_request`, `not_found`, `query_error`, `conflict`,
`unauthorized`, `forbidden`, `embedder_unavailable`, `canceled`, `timeout`, `internal_error`); `request_id` is the request's
`X-Request-Id` header when one was sent, otherwise a server-generated id, echoed back as the
`X-Request-Id` response header — when a message says the underlying cause is in the server log
under this id, this is the id. The full list of operations and their request schemas is in the OpenAPI document (`GET /v1/openapi.json`).

Insert a record:

```bash
base='{{ .BaseURL }}'
curl -s -X POST "${base%/}/v1/insert" \
  -H 'Content-Type: application/json' \
  -d '{"namespace":"research","table":"findings","records":[{"title":"auth flow","done":true}]}'
```

Success envelope:

```json
{"ok":true,"data":{"ids":[1],"inserted":1}}
```

The same call with a typo'd field name returns the error envelope:

```json
{"ok":false,"error":{"code":"invalid_request","message":"unknown field \"titel\" on table findings (see describe_table)","request_id":"7c3e9a1f5b2d4e8a9c0f6b1d3e5a7c9f"}}
```

Searches answer with `results`, where `query` and `read_rows` answer with `rows`:

```json
{"ok":true,"data":{"results":[{"id":1,"created_at":"2026-09-23T16:19:06.326Z","title":"auth flow","body":"token expiry not checked","_score":1.2145}],"truncated":false,"limit":10}}
```

`search_vector` adds `_score` to each result and reports `skipped_vectors`:

```json
{"ok":true,"data":{"results":[{"id":1,"created_at":"2026-09-23T16:19:06.326Z","title":"auth flow","body":"token expiry not checked","_score":0.9046}],"truncated":false,"skipped_vectors":0,"limit":10}}
```

## JSON-RPC fallback

When the `dolmen` tools are not connected and no user is available to re-run the connection
command, drive the MCP endpoint directly: JSON-RPC 2.0 request objects, one per `POST` to
`{{ .MCPURL }}` (`Content-Type: application/json`). The endpoint is stateless — no session id,
no handshake order — so every request stands alone and a `tools/call` works without a prior
`initialize`. Tool names, input schemas, and `structuredContent` results are exactly what a
connected MCP client sees; for plain one-shot calls the raw HTTP operations above are simpler.

Initialize:

```bash
mcp='{{ .MCPURL }}'
curl -s -X POST "$mcp" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agent","version":"1.0"}}}'
```

List every tool with its input schema (single page, no cursor):

```bash
mcp='{{ .MCPURL }}'
curl -s -X POST "$mcp" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

Call a tool — `arguments` is the tool's input, and the result data arrives unwrapped in
`result.structuredContent`:

```bash
mcp='{{ .MCPURL }}'
curl -s -X POST "$mcp" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_tables","arguments":{"namespace":"research"}}}'
```

```json
{"id":3,"jsonrpc":"2.0","result":{"content":[],"isError":false,"structuredContent":{"tables":["findings"]}}}
```

A failed call is not an HTTP error: the result carries `"isError":true` and the error object
(`{"code","message","request_id"}`) as JSON text in `content[0].text`.

## Live changes: the subscribe stream (SSE)

`GET /v1/subscribe` is `wait_for`'s push counterpart: one long-lived `text/event-stream` connection
that replays the namespace's durable change log from a cursor and then delivers every live commit
as it happens. It is HTTP-only — there is no MCP tool for it (`wait_for` carries the same feed
semantics request/response); use it when your host holds connections (dashboards, watchers), and
`wait_for` when you are a one-shot agent. `capabilities`'s `subscribe` field reports whether the
server offers it.

Bash:

```bash
base='{{ .BaseURL }}'
curl -sN "${base%/}/v1/subscribe?namespace=research"
```

Query parameters — the same contract as `changes_since` (names are trimmed and lowercased like
every `/v1` call):

- `namespace` (required) — the feed to subscribe to. A namespace that does not exist is an
  in-stream `not_found` error: the stream never creates one, as no read does, unlike the
  write ops.
- `table` (optional) — filter to that table's feed. An explicitly empty value is rejected — omit
  the parameter for the namespace-wide feed. A table-filtered feed has its own cursors.
- `cursor` (optional) — an opaque resume token or the literal `begin`. Omitted = start at the
  current head (future commits only). An explicitly empty value is rejected.

Request-shape failures (wrong method, an omitted `namespace` parameter, an empty `table` or
`cursor` value) are ordinary HTTP errors — the standard envelope — before any stream bytes go
out; everything
the stream discovers afterwards arrives as an in-stream `error` event, because an open
`text/event-stream` response can no longer carry an HTTP status.

### Frames

Each frame is a named event (`event: <name>`) whose `data` is one compact JSON line:

| Event | `data` | Meaning |
|---|---|---|
| `ready` | `{"cursor":"..."}` | The replay→live boundary. Replayed `change` frames (when resuming behind the head) arrive first, then `ready`, then live frames. Its cursor is the last replayed change's cursor, or the head at registration when nothing replayed — the resume point a fresh subscriber holds; reconnecting with it continues exactly there, gap-free. A stream that ends during replay never reaches `ready`; its `close` frame carries the reached cursor instead. |
| `change` | `{"cursor":"...","table":"...","row_id":N,"kind":"insert\|update\|delete"}` | One changed row per frame — a batch write (multi-record `insert`, filter-matched `update`/`delete`) mints one change-log record per affected row — delivered in commit order; the same four-field identity projection as `changes_since` changes. Re-read the row by id (`read_rows`, or `query` with a `WHERE id = ?`); a `delete` change names a row that is already gone. |
| `close` | `{"cursor":"..."}` | Sent before every server-initiated terminal, carrying the last-delivered cursor — the reconnect token. |
| `error` | `{"ok":false,"error":{"code","message","request_id"}}` | The terminal frame: the standard error envelope as the event data. Nothing follows it. |

An idle stream is not silent: between change deliveries the server sends a `: keepalive`
comment frame every 20 seconds (the 15–30 s heartbeat band's chosen point). It is an SSE
comment line — no event name, no data — so every event parser ignores it, and it never carries
or advances the cursor; only `change` frames do. Treat keepalive receipt as liveness: a
connection that delivers neither a change nor a keepalive for a couple of intervals (~40 s) is
dead — close it and reconnect from your last cursor.

### Terminal causes and their recipes

Every server-initiated terminal is a `close` frame followed by a teaching `error` event;
registration failures — an unknown or foreign cursor, a missing namespace, a missing table —
send only the `error` event, since nothing was delivered. The messages are the recipes, verbatim:

- Unknown or expired cursor: `cursor is unknown or past the change-log retention window
  (-change-retention, default 168h); catch up by reconnecting with no cursor to resume from the
  current head, or with cursor=begin to replay retained history`
- Cursor from another feed: `cursor was minted on a different feed (a specific table's, or the
  namespace-wide feed); pass it only to the feed you received it from — honoring it elsewhere
  would silently skip events — or start fresh with no cursor / cursor=begin`
- Buffer overflow: `subscription buffer overflow: commits arrived faster than this stream drained
  them; reconnect from the cursor in the preceding close frame — the durable log is the catch-up
  path, the buffer never was`
- Target ended: `the subscription's target ended (a dropped table, or a dropped or replaced
  namespace); reconnect against the current target — a same-named successor is a different feed`
- Authorization revoked (code `forbidden`): `subscription authorization was revoked; reconnect once authorization is
  restored`. The server rechecks access on every change it delivers and on every keepalive (every 20 seconds),
  so a revoked caller's stream ends within about 20 seconds even when nothing is being written.
- Own-row feed over unlabelled history: `this feed still retains changes recorded before rows
  carried an owner, and a caller restricted to their own rows cannot be shown them or told they
  were skipped; subscribe without a cursor to start at the current head, or ask for the read verb
  on the table, which lifts the scope`
- Subscription age bound: `subscription reached the maximum subscription age
  (-max-subscription-age, default 30m); reconnect from the cursor in the preceding close frame to
  resume exactly where this stream ended — the fresh connection re-asserts your credentials`

Recovery is one rule for the cursor-reusable terminals — buffer overflow, revoked, age bound:
reconnect with the `close` frame's cursor (or the newest you hold, from `ready` or the last
`change`, when the connection died without one) and the new stream replays everything after it,
then goes live — no gaps, no duplicates. Target ended is the close-bearing exception: the feed
you were reading is gone (a dropped table's feed, or a dropped/replaced namespace whose cursor
store was deleted with it), so its cursor cannot resume anything — reconnect against the current
target and establish a fresh cursor (no cursor, or `cursor=begin`), as its message says; a
same-named successor is a different feed. The registration failures are the other exception:
their cursor was rejected — reconnect with no cursor (head) or `cursor=begin`, never the
rejected token. The own-row feed over unlabelled history is the one terminal that can arrive
either way — at registration, or mid-replay if your grant narrows to your own rows while the
stream is catching up — and `cursor=begin` will be refused again, so reconnect with no cursor.

## Working rules

1. **Never invent a schema.** If a table is missing or its shape is unknown, call `list_tables` and `describe_table`. If a table does not exist or is the wrong shape, stop and surface the need. Do not call `infer_schema`, `create_table`, or `migrate` — those are in the `dolmen-admin` skill.
2. **Check before writing.** Call `list_namespaces` then `list_tables` first; reuse an existing namespace or table when one fits.
3. **Inspect when the schema is unknown or may have changed.** Call `describe_table` to get its schema, version, and row count. Use that to build correct `query` / `search_fulltext` / `search_vector` calls and to avoid inventing field names. Its `row_count` is kept up to date by every write, so reading it costs the same on a table of any size; still cache the schema for the session instead of calling it before every read or write.
4. **Record as you go.** After finishing a meaningful unit of work, `insert` a record summarizing it (what/where/outcome). Future sessions recall it via search.
5. **Read with the cheapest tool that answers the question:** `describe_table` → exact lookups via `query` (SQL, read-only) → `search_fulltext` for keyword recall → `search_vector` for meaning-based recall.
6. **Never write SQL that mutates.** `query` rejects it by design; use `insert` and `delete` for changes. If you need to update or upsert records, or change a table's schema, ask the user to switch to the `dolmen-admin` skill.
7. **Do not fork tables.** When a table is the wrong shape, do not create a parallel v2 table. Report the mismatch and ask the user whether to use the `dolmen-admin` skill to migrate or create a new table.

## Quick reference

- Core tools: `describe_server`, `list_namespaces`, `list_tables`, `describe_table`, `insert`, `query`, `search_fulltext`, `tokenize` (takes `namespace`, `table` and `text`), `search_vector`, `changes_since`, `wait_for`, `delete`.
- Schema types: `string`, `text` (long, searchable), `number`, `boolean`, `timestamp`, `json`, `vector` (caller-supplied embeddings; requires a separate `"dim": N` property on the field), and `secret` (a string encrypted at rest; see below).
- `secret` fields read back as the fixed mask `"••••"` (or `null` when unset) in every read: `read_rows`, both searches, and `query`. To get the plaintext, name the field in `reveal` on `read_rows`, `search_fulltext` or `search_vector` (`"reveal": ["api_token"]`); `query` never reveals. SQL (`query` and search `filter`s) sees only ciphertext, so never filter or join on a secret field. Under `-auth on` reveal needs the `reveal` verb (a `forbidden` error names it; `admin` does not imply it), and every reveal is audit-logged without the value. Writing a secret needs the server's secret key; without it the write is refused.
- Field annotations: `fulltext: true` (FTS5 search), `vectorize: true` (server embeds this field — enables `search_vector` with `text`; the built-in `local` provider is enabled by default; set `DOLMEN_EMBED_PROVIDER=openai` for an external endpoint, or `none` to disable server-side embeddings), `required: true`, `enum: [values]` (closed vocabulary for a string field — writes with any other value are rejected naming the field, the value, and the allowed list; exact match, no case folding; a declared `default` must be a member), `shape` on a `json` field (`object`, `array`, `array<string>`, `array<number>`, `array<boolean>` or `array<object>` — writes of any other shape are rejected naming the field, the expected shape and what arrived, so send tags as `["db","sqlite"]`, never `"db,sqlite"`; omit it for free-form JSON).
- `describe_server` reports the embedding provider status without attempting a write: `provider` (`none` / `local` / `openai`), `model`, the `identity` that pins vectorized tables, `usable`, and — for the `local` provider — `model_cached`, whether the model weights are complete on the server so no first-use download is needed (`false` means the first vectorized write or `text` search downloads a Hugging Face model, so it can take ten seconds or more and can fail transiently — retry, or pre-seed; with `DOLMEN_EMBED_MODEL` naming a directory, `false` means the directory is incomplete and no download repairs it). `vectorize` fields and `search_vector` `text` queries fail while `usable` is false; a table whose `embed_space` (see `describe_table`) differs from `identity` was embedded by a different provider/model and rejects inserts and text searches until it is re-embedded.
- `query` parameters: use `?` placeholders and pass `args` — never interpolate values into SQL.
- `truncated: true` means the response left results out. On `query`, `search_fulltext` and `search_vector`, more exist beyond the page, cut either by `limit` (1,000 rows by default and at most on `query`; 10 by default and 200 at most on the searches) or by the 32 MiB response budget, so fetch the next page with `offset`. On `read_rows` only the budget cuts, so retry with fewer ids.
- Both searches score every hit as `_score`, higher being more relevant, and return results in that order. The two scales are different and engine-specific — full-text relevance is the engine's own ({{ if eq .Dialect "postgresql" }}PostgreSQL `ts_rank_cd`{{ else }}FTS5 BM25, negated so higher wins{{ end }}), vector `_score` is cosine similarity — so compare scores only within one query's results, never across queries, tables or servers, and never threshold full-text `_score` against a fixed number.
- `search_fulltext` and `search_vector` accept an optional `filter` — a SQL WHERE expression over the table's columns with `?`-bound `args` (same quoting rules as `query`) — applied before ranking.
- `delete` requires a `filter` (SQL WHERE expression); use `"1=1"` only when you truly mean everything.
{{ if eq .Dialect "postgresql" }}- **This server is PostgreSQL-backed.** `query`, and `filter` when authentication is off, are
  PostgreSQL SQL (`capabilities` reports `query_dialect`/`filter_dialect` as `postgresql`). SQLite
  functions such as `date()`, `strftime()`, `julianday()`, `iif()`, `instr()` and `ifnull()` do
  not exist here; use `CASE`, `coalesce`, `strpos`, `extract`, `date_trunc` and `to_char`.
  `timestamp` fields are stored as ISO-8601 text, so cast before date arithmetic, and pin the zone:
  `extract(year from (published_at::timestamptz AT TIME ZONE 'UTC'))`. Only an allowlist of
  standard functions is accepted; the error names anything refused. Per-day counts over the last two
  weeks: `SELECT date_trunc('day', started_at::timestamptz AT TIME ZONE 'UTC') AS day, count(*) FROM
  meetings WHERE started_at::timestamptz > now() - interval '14 days' GROUP BY 1 ORDER BY 1`.
{{ end }}- `drop_table` / `drop_namespace` are irreversible deletions and are **not** part of this skill; do not use them. Ask the user to use `dolmen-admin` if a table or namespace must go.
- `insert` with an `idempotency_key` (any unique string) makes retries replay the original ids; the same key with different records is rejected. Use printable ASCII keys (`[ -~]`) up to 256 bytes.
- Every table has implicit `id` and `created_at` columns; `SELECT *` includes them.
- Results honor declared field types in every read (`query`, `search_fulltext`, `search_vector`): `boolean` → `true`/`false`, `json` → the decoded value, `vector` → a number array, `secret` → the mask `"••••"` unless revealed, SQL `NULL` → `null`. In `query`, coercion is by result-column label (aliases count as their label); labels that match no declared field fall back to raw values (blobs as base64).
- The hidden `_embedding` column (from `vectorize`) is excluded from `SELECT *` and search results; pass `include_hidden: true` to a search when you really need it. Naming it in `query` SQL (outside string literals and comments) also works where the backend exposes it to caller SQL, but that is backend-dependent — `include_hidden: true` is the portable way to reach it.
- Vector search results carry `_score` (cosine similarity; higher is closer). Judge a score against the other results of the same query, not against a fixed cutoff: with the default `local` model, relevant matches commonly score only about 0.2 to 0.6.
- `search_vector` has two query forms with different reach: `text` (server embeds it) searches only the vectorize `_embedding` space — a table without a `vectorize` field rejects `text`; `vector` (raw numbers) searches any `vector` column, and only you know which embedding space produced both the stored and the query vectors, so keep them from the same model.
  **Rows whose `vectorize` source is `null`/empty/missing have `_embedding` `null` and are silently excluded from any `search_vector` that searches `_embedding` (a `text` query, or a raw `vector` query with `column` omitted or set to `_embedding`). If recall matters, and the backend exposes `_embedding` to caller SQL, call `query` with `SELECT COUNT(*) FROM <table_name> WHERE _embedding IS NULL AND (<same filter>)` (substitute the table name; bind the same `args`; drop the `AND (...)` clause when no filter is used) to find unembedded rows eligible for the search; if that column is not reachable, or you compare counts instead, do it against `SELECT COUNT(*) FROM <table_name> WHERE <same filter>` after exhausting all pages with `min_score` unset (omit the WHERE clause when no filter is used).**
- `skipped_vectors` in a `search_vector` response counts stored vectors that were corrupt or dimension-mismatched and could not be scored; **it does not count rows with a `null`/empty/missing `vectorize` source — those rows are silently excluded and will not raise `skipped_vectors`.**
- `changes_since` replays a namespace's durable change log instead of polling tables: each call returns the changes committed after the cursor plus `next_cursor`. Omit `cursor` to start at the current head (nothing replays; keep the returned `next_cursor` and later calls deliver only new commits), or pass `"begin"` to replay retained history. An optional `table` filters to that table. Changes carry `cursor`/`table`/`row_id`/`kind` only — re-read row content by id with `query` (`SELECT * FROM <table> WHERE id = ?`). A cursor older than the change-log retention window (default 7d) is rejected with an error telling you to restart from the head (omit `cursor`) or `"begin"`; cursors are per-feed, so a cursor from a `table`-filtered call only works on that same feed. Cursor tokens are minted per emission: a change re-read later carries a fresh token for the same commit, so the same commit yields different tokens on different reads — while a read that emits nothing returns the cursor you passed unchanged (the `wait_for` idle contract). Treat a token as a resume handle, never as an event id — and no frame field is one either: the same row updated twice yields two changes with identical `table`/`row_id`/`kind`. No stable per-event identifier is exposed; make processing idempotent and persist the cursor atomically with your side effects instead of deduplicating on frame content.
- `wait_for` REPLACES polling: one call blocks server-side until a change commits after the cursor (or `timeout_ms` elapses, default 30000, max 60000), then returns exactly a `changes_since` page. A timeout is an **empty page plus the unchanged `next_cursor` — never an error**: pass `next_cursor` straight back into the next `wait_for` and loop. `timeout_ms: 0` is a cheap conditional poll (returns immediately). Same feed semantics as `changes_since` (`cursor` resume, `"begin"`, optional `table` filter); never re-derive the head between waits — always resume from the returned cursor. Like every read, a wait never creates its namespace — a missing one is `not_found` (create it first, then wait).
- `GET /v1/subscribe?namespace=...` is the stream counterpart of `wait_for` — HTTP-only (`text/event-stream`, no MCP tool): replay the log from `cursor`/`"begin"`/the head, then live `change` frames, one per changed row, in commit order. `ready` (replay→live boundary) and `close` (before every server-initiated terminal) carry cursors — resume from the `close` cursor after overflow/revoked/age-bound, and start fresh (no cursor, or `begin`) after target-ended or a rejected cursor. See "Live changes: the subscribe stream" above.

## Agent-critical caveats

### Quoting and placeholders

- `query` only accepts read-only `SELECT`/`WITH` statements. Bind all values with `?` and pass them in `args`. Identifiers and table names cannot be bound with `?`; write them directly from `list_tables`/`describe_table` and never let untrusted input choose them. `query` only checks that the statement is read-only, not that the identifiers are safe.
- SQL string literals use single quotes (`'value'`), escaped by doubling (`'can''t'`). Prefer `?`.
- Double quotes are for SQL identifiers, not string values.
- `search_fulltext` takes a raw {{ if eq .Dialect "postgresql" }}full-text search{{ else }}FTS5 `MATCH`{{ end }} expression in `query`; it is **not** SQL, so do not wrap the whole expression in single quotes. Punctuation inside a term is not searchable text: a hyphenated slug, SKU or compound word (`gpt-4`, `e-mail`) must go in double quotes (`"gpt-4"`), or the bare `-` is rejected.

{{ if eq .Dialect "postgresql" }}### Full-text search syntax (PostgreSQL)

This server indexes `fulltext` fields with PostgreSQL's `english` text-search configuration
(stemming, stop words and accent handling are PostgreSQL's) and ranks with `ts_rank_cd`, highest
first, ties by id. Ranking is PostgreSQL's own and differs from a SQLite-backed server's BM25.

Supported in `query`:

- `payment refund` — both terms (implicit AND); `AND`, `OR` and binary `NOT` (`payment NOT refund`)
  work as written, uppercase only.
- `"refund processed"` — an exact phrase; also double-quote terms containing punctuation.
- `pay*` — a prefix term.
- Parentheses group expressions.

Refused, each with an error naming the alternative: the `field:term` column filter (use the
`filter` parameter instead), `NEAR()` (use a quoted phrase for adjacent words), the `^`
first-token operator and `+` adjacency. A query made only of stop words (`the`, `and`) matches
nothing.

The optional `filter` parameter is separate from the search `query`: it is SQL over the table's
columns (a WHERE expression with `?`-bound `args`) and selects which rows may match, before ranking.

{{ else }}### Full-text (FTS5) search syntax

Dolmen indexes `fulltext` fields with SQLite FTS5 using the `porter` stemmer over the `unicode61`
tokenizer: case-insensitive, diacritic-insensitive for most Latin characters (some non-Latin or
multi-diacritic characters may not normalize), **stemmed** — the index and the query both reduce
English words to stems, so plural/inflected terms just match (`payments` ↔ `payment`, `refunds` ↔
`refund`). Most punctuation (including hyphens) is a token boundary.

**Stemming notes (porter is a suffix-stripper, not a lemmatizer):** it collapses inflections of the
same root but not different derivations — `paying`/`pays`/`paid` stem to `pai`/`paid` and do **not**
match `payment`. Phrases match on stems (`"payments were"` matches `the payments were refunded`).
Prefix queries operate on stems: `pay*` stems to `pai*`, matching `paid`/`paying`/`pays` but not
`payment`. To see what a word stems to, call `tokenize` with the table and the word:
`{"namespace": "…", "table": "…", "text": "overheating"}` returns `{"terms": ["overh"]}`, so
search `overh*`, not `overheat*`. Stemming is English-focused. Tables created before stemming became the default keep
their exact-token index (they keep working); reindex one with `migrate`:
`{"op": "set_fulltext", "name": "<fulltext field>", "value": true}` — re-asserting `true` on an
already-indexed field rebuilds the index under the current tokenizer.

**CJK limitation:** `unicode61` does not segment CJK text — it breaks tokens only at whitespace and
punctuation. Chinese/Japanese are usually written without spaces, so an uninterrupted run of CJK
characters is indexed as one opaque token: a `search_fulltext` term for a word or substring *inside*
the run silently matches nothing (no error; the rows are stored and `LIKE`-queryable via `query`).
Whole-run terms, prefix queries (`中华*`), and space-delimited Korean still tokenize and match. For
keyword recall over space-less CJK text, fall back to vector search over an embedding column
(`vectorize: true` + `search_vector(text=...)`) or `query` with `LIKE`.

- `payment` — one token (`payments` matches the same stem).
- `payment gateway` — implicit `AND`.
- `payment OR gateway`.
- `payment NOT gateway`.
- `title:payment` — only in the `title` fulltext field.
- `{title body}:payment` — any of those fields.
- `"foo bar"` — phrase (adjacent tokens, matched on stems). Phrases match token adjacency, not literal punctuation.
- `"foo-bar"` — double-quote terms that contain spaces or punctuation; bare `foo-bar` is read by FTS5 as a column filter and errors.
- `pay*` — prefix match, applied to the stemmed term (`pay*` → `pai*`).
- `NEAR(payment refund)` — proximity search (default near span). Use the group form
  `NEAR(term1 term2 ...)`; `term1 NEAR(term2)` is parsed as an implicit `AND` and does not enforce proximity.
- Terms like `"can't"` must be in double quotes; bare single quotes are a syntax error.

Results are ordered by FTS5 `rank` (BM25 by default): more relevant rows have a lower — more negative — value and are returned first. The rank value itself is not returned.

The optional `filter` parameter is separate from the MATCH `query`: it is regular SQL over the table's columns (a WHERE expression with `?`-bound `args`, like `delete`'s filter) and selects which rows may match, before ranking.

{{ end }}### Vectors and semantic recall

- `vector` fields accept JSON number arrays of the declared `dim`; stored as float32 blobs, returned as `[]float64`.
- `vectorize: true` on a string/text field stores one embedding per non-empty row in `_embedding`. Only one field per table can be vectorized.
- **Vector search has silent recall holes.** Rows with `null`, empty string, or missing values in the vectorized field have `_embedding` NULL and are silently excluded from any `search_vector` that searches `_embedding` — whether it is a `text` query or a raw `vector` query with `column` omitted or set to `_embedding`. `skipped_vectors` does NOT count those rows — it only counts stored vectors that are corrupt or dimension-mismatched and could not be scored. If recall matters, and the backend exposes `_embedding` to caller SQL, call `query` with `SELECT COUNT(*) FROM <table_name> WHERE _embedding IS NULL AND (<same filter>)` (substitute the table name; bind the same `args`; drop the `AND (...)` clause when no filter is used) to find unembedded rows eligible for the search. If that column is not reachable, or you instead compare counts, compare against `SELECT COUNT(*) FROM <table_name> WHERE <same filter>` after exhausting all pages with `min_score` unset (omit the WHERE clause when no filter is used) — otherwise pagination, `min_score`, offsets, filters, and response truncation can make embedded rows look missing.
- `search_vector(text=...)` embeds the query `text` with the configured provider and searches only the vectorize `_embedding` space — a table without a `vectorize` field rejects `text`. `search_vector(vector=[...])` supplies a query vector directly and may search any vector column.
- `column` applies to `vector` queries: it names the stored-vectors column and defaults to `_embedding` (if a vectorized field exists) or the first declared `vector` field. The query and stored vectors must come from the same embedding space. For `_embedding` (from `vectorize`) this means the same provider/identity; for caller-supplied `vector` fields it means the same model used for the stored and query vectors.
- Each result has `_score`: cosine similarity, higher is closer, typically `0`–`1` for positive embeddings (mathematically `-1`–`1`).
- `_embedding` is hidden from `SELECT *` and search results unless `include_hidden: true` is passed, or the SQL names it explicitly on a backend that exposes it to caller SQL.

### Id, `created_at`, and stability

- Every row has `id` and `created_at`. You cannot supply them; they are assigned on insert and returned in reads.
- `id` is `AUTOINCREMENT` — monotonically increasing and never reused after deletes — so it is safe to key off across sessions.
- `created_at` is a UTC millisecond ISO string, e.g. `2026-09-03T12:34:56.123Z`. Use string comparisons or {{ if eq .Dialect "postgresql" }}cast it with `::timestamptz` as described above{{ else }}SQLite date/time functions{{ end }}.

### Limits and guardrails

| Resource | Limit | Behavior |
|---|---|---|
| Namespace path | 1–3 segments (`a/b/c`), each `^[a-z0-9][a-z0-9_-]{0,63}$` (max 64 chars per segment) | rejected |
| Table / field name | `^[a-z][a-z0-9_]{0,63}$` (max 64 chars); reserved names (`id`, `created_at`, `_embedding`, `_score`, `_rank`, `rowid`) are rejected, SQL keywords (`key`, `order`, `group`, `from`, `values` and the rest of SQLite's list) are rejected with a suggested replacement, and a field named `rank` is rejected when `fulltext: true` (reserved by the FTS5 index); table also cannot contain `__fts` or start with `sqlite_` | rejected |
| Table fields | 100 user-defined fields (not counting the implicit `id`, `created_at`, `_embedding` columns) | rejected |
| Records per `insert` / `upsert_by_key` | 1,000 | rejected |
| Ids per `read_rows` | 1,000 | rejected |
| Natural key fields per `upsert_by_key` | 8 | rejected |
| Idempotency key length | 1–256 bytes; use printable ASCII; omit the field for a non-idempotent insert | empty and over-256-byte keys are rejected; the JSON Schema enforces non-empty printable ASCII for schema-validating clients |
| Vector dimension (declared `vector` fields) | 1–4096 | rejected |
| `search_vector` query vector | at most 4096 numbers | rejected before the request is decoded |
| Search `limit` | default 10, max 200 | omit `limit` for the default of 10; the tool schema enforces 1–200 for schema-validating clients, and the server clamps values above 200 to 200 (0 or negative selects the default on direct `/v1` calls); every search response reports the `limit` it applied |
| `query` result rows | 1,000 | truncated with `truncated: true` |
| `query` / search result size | 32 MiB | first row over budget errors; later rows truncate; a single BLOB value over 32 MiB always errors |
| A single value built by SQL (in `query` or a search `filter`), SQLite engine | 64 MiB | `query_error` before the value is built |
| Request body | 32 MiB, sent within the server's read limit (2 minutes by default) | over 32 MiB is rejected; a body that arrives too slowly is `timeout` (408) |
| Time per operation | set by the server, 2 minutes by default; `wait_for` gets its `timeout_ms` on top | `timeout` (504): narrow a read (a filter, a smaller limit) and retry it; a write may or may not have committed, so check with a query before retrying it |
| `query` / search filter `args` | 100 | rejected |

Vector search is exact: it scores every row. With the server's vector cache warm (SQLite engine), a search takes about 1 ms per 20,000 rows at 384 dimensions and 3 ms at 1,536, including on `row_access: own` tables. A `filter` adds the time SQLite takes to read the table and evaluate it, roughly 3 ms per 1,000 rows at 384 dimensions, unless it only touches `id`. A table too large for the cache reads vectors from disk, about a tenth of a second per 50,000 rows at 384 dimensions.

Validation notes:

- `number` becomes `int64` or `float64`: integral values within the int64 range become `int64`; a JSON number outside the int64 range is stored as `float64`, which loses precision — it is NOT rejected (`9223372036854775808` reads back as `9223372036854776000`). The separate rejection of unsigned values above `MaxInt64` applies only to the Go library, where the value arrives typed.
- `timestamp` must be a parseable ISO/RFC3339 string.
- `vector` must be a number array of exactly the declared `dim`; `NaN`/`Inf` are rejected. The 4096-dimension cap applies only to declared `vector` fields; `vectorize` records the provider's returned dimension.
- Unknown field keys are rejected. Missing or `null` required fields are rejected on `insert`. Fields
  may carry a declared `default` (shown by `describe_table`): an insert omitting such a field stores
  the default instead of NULL; an explicit `null` still stores NULL. A timestamp field may declare
  `default: "now()"` — the server stamps its current write time on each insert that omits the field.
- Namespace paths and table names are trimmed and lowercased before validation on direct `/v1` requests — a namespace per segment, so `"namespace":" Production / EU "` operates on `production/eu`. The MCP tool schemas require already-canonical names — always send trimmed lowercase names.
- `query` accepts only `SELECT`/`WITH`, rejects embedded semicolons (trailing semicolons are accepted), and binds at most 100 `args`.
- `search_vector` with `text` requires a provider and searches only the server-managed `_embedding` column produced by a `vectorize: true` field — the provider identity must match the one that embedded the table, and a `text` query naming a declared `vector` column is rejected. Searches with a caller-supplied `vector` need no provider and are not checked against any embedding space — only you know which model produced the stored and query vectors. The built-in `local` provider is enabled by default; `describe_server` reports the active provider, its identity, and whether server-side embedding is usable.
- `insert` with an `idempotency_key`: the same key + same records replays the original ids; the same key with different records is rejected. Use printable ASCII keys (`[ -~]`) up to 256 bytes. Never regenerate a timestamp in a retried record — the body must be byte-identical to replay. A timestamp column a retry would stamp (e.g. `updated_at`) belongs in the table as `default: "now()"` (declared at `create_table` via the `dolmen-admin` skill) and omitted from records, so the server stamps it and the retry replays.

## Typical flows

Store session findings:

```
list_tables(namespace="research")                       → review names →
describe_table(namespace="research", table="findings")
  → missing:
    stop and ask the user; do not create tables with this skill
  → exists but wrong shape:
    stop and ask the user; do not create a v2 table
  → exists and fits:
    insert(namespace="research", table="findings", records=[{...}])
```

Recall in a later session:

```
describe_table(namespace="research", table="findings")                  # confirm fields
search_fulltext(namespace="research", table="findings", query="auth")   # needs a fulltext field;
                                                                       # search_vector with text needs a
                                                                       # vectorize field plus a provider
query(namespace="research", sql="SELECT * FROM findings WHERE created_at >= ? ORDER BY created_at DESC", args=["2026-09-01"])
```

Catch up on what changed while you were away (instead of re-scanning tables):

```
changes_since(namespace="research")                    # first call: empty page + the head cursor
insert(...)                                            # other writers commit
changes_since(namespace="research", cursor=<next_cursor>)   # exactly the new commits, in order
# a cursor that outlived the retention window is rejected — restart from the head (omit cursor)
```

Sleep until something changes — the poll-replacement pattern (one tool call per wait, the server holds it, timeout is an empty page plus the cursor to re-wait from, never an error):

```
page = wait_for(namespace="research", timeout_ms=0)         # immediate: pin the head (a fresh head start
                                                             # has nothing to deliver — an empty page)
while working:
    page = wait_for(namespace="research", cursor=page.next_cursor)   # blocks up to 30s (default)
    for change in page.changes:                              # every page is processed before the next
        # change.table names the table the commit landed in — server-validated, and table names
        # cannot be ?-bound, so interpolating the feed's own value (never caller input) is correct:
        read the row: query(namespace="research", sql=f"SELECT * FROM {change.table} WHERE id = ?", args=[change.row_id])
    # an empty page means nothing happened: just re-wait
```
