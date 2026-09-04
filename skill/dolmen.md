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
base="{{ .BaseURL }}"
curl -s "${base%/}/healthz"
```

Windows PowerShell:

```powershell
$base = "{{ .BaseURL }}"
curl.exe -s "$($base.TrimEnd('/'))/healthz"
```

Should return `{"status":"ok"}`. If the server is not running, do not improvise — start it first.

### Connect the skill

Bash:

```bash
claude mcp add --transport http dolmen "{{ .MCPURL }}"
```

Windows PowerShell:

```powershell
claude mcp add --transport http dolmen "{{ .MCPURL }}"
```

The `dolmen` tools then appear in `tools/list` with full input schemas. The endpoint can also be
read from the environment: `DOLMEN_URL` (default `{{ .BaseURL }}`).

If the `dolmen` MCP tools are not connected, do not improvise — ask the user to re-run the
connection command above. MCP servers cannot be hot-loaded into an already-running session; when
no user is available to re-run it, use the JSON-RPC fallback below instead.

## Raw HTTP

Every tool in this skill is also a plain HTTP operation: `POST /v1/{operation}` with the tool's input
as the JSON body (`Content-Type: application/json`). Responses are enveloped — success is
`{"ok":true,"data":...}` and failure is `{"ok":false,"error":{"code","message"}}` with a stable
machine-readable `code` (`invalid_request`, `not_found`, `query_error`, `conflict`, `forbidden`,
`internal_error`). The full list of operations and their request schemas is in the OpenAPI document (`GET /v1/openapi.json`).

Insert a record:

```bash
base="{{ .BaseURL }}"
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
{"ok":false,"error":{"code":"invalid_request","message":"unknown field \"titel\" on table findings (see describe_table)"}}
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
mcp="{{ .MCPURL }}"
curl -s -X POST "$mcp" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agent","version":"1.0"}}}'
```

List every tool with its input schema (single page, no cursor):

```bash
curl -s -X POST "$mcp" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

Call a tool — `arguments` is the tool's input, and the result data arrives unwrapped in
`result.structuredContent`:

```bash
curl -s -X POST "$mcp" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_tables","arguments":{"namespace":"research"}}}'
```

```json
{"id":3,"jsonrpc":"2.0","result":{"content":[],"isError":false,"structuredContent":{"tables":["findings"]}}}
```

A failed call is not an HTTP error: the result carries `"isError":true` and the error object
(`{"code","message"}`) as JSON text in `content[0].text`.

## Working rules

1. **Never invent a schema.** If a table is missing or its shape is unknown, call `list_tables` and `describe_table`. If a table does not exist or is the wrong shape, stop and surface the need. Do not call `infer_schema`, `create_table`, or `migrate` — those are in the `dolmen-admin` skill.
2. **Check before writing.** Call `list_namespaces` then `list_tables` first; reuse an existing namespace or table when one fits.
3. **Inspect when the schema is unknown or may have changed.** Call `describe_table` to get its schema, version, and row count. Use that to build correct `query` / `search_fulltext` / `search_vector` calls and to avoid inventing field names. Avoid calling it before every read or write on large tables — it runs a full `count(*)`, so cache the schema for the session.
4. **Record as you go.** After finishing a meaningful unit of work, `insert` a record summarizing it (what/where/outcome). Future sessions recall it via search.
5. **Read with the cheapest tool that answers the question:** `describe_table` → exact lookups via `query` (SQL, read-only) → `search_fulltext` for keyword recall → `search_vector` for meaning-based recall.
6. **Never write SQL that mutates.** `query` rejects it by design; use `insert` and `delete` for changes. If you need to update or upsert records, or change a table's schema, ask the user to switch to the `dolmen-admin` skill.
7. **Do not fork tables.** When a table is the wrong shape, do not create a parallel v2 table. Report the mismatch and ask the user whether to use the `dolmen-admin` skill to migrate or create a new table.

## Quick reference

- Core tools: `list_namespaces`, `list_tables`, `describe_table`, `insert`, `query`, `search_fulltext`, `search_vector`, `delete`.
- Schema types: `string`, `text` (long, searchable), `number`, `boolean`, `timestamp`, `json`, and `vector` (caller-supplied embeddings; requires a separate `"dim": N` property on the field).
- Field annotations: `fulltext: true` (FTS5 search), `vectorize: true` (server embeds this field — enables `search_vector` with `text`; needs an embedding provider on the server: the built-in `local` one or an external endpoint), `required: true`.
- `query` parameters: use `?` placeholders and pass `args` — never interpolate values into SQL.
- `delete` requires a `filter` (SQL WHERE expression); use `"1=1"` only when you truly mean everything.
- `drop_table` / `drop_namespace` are irreversible deletions and are **not** part of this skill; do not use them. Ask the user to use `dolmen-admin` if a table or namespace must go.
- `insert` with an `idempotency_key` (any unique string) makes retries replay the original ids; the same key with different records is rejected. Use printable ASCII keys (`[ -~]`) up to 256 bytes.
- Every table has implicit `id` and `created_at` columns; `SELECT *` includes them.
- Results honor declared field types in every read (`query`, `search_fulltext`, `search_vector`): `boolean` → `true`/`false`, `json` → the decoded value, `vector` → a number array, SQL `NULL` → `null`. In `query`, coercion is by result-column label (aliases count as their label); labels that match no declared field fall back to raw values (blobs as base64).
- The hidden `_embedding` column (from `vectorize`) is excluded from `SELECT *` and search results; reference it in the SQL (outside string literals and comments) or pass `include_hidden: true` to a search when you really need it.
- Vector search results carry `_score` (cosine similarity; higher is closer).
- `search_vector` has two query forms with different reach: `text` (server embeds it) searches only the vectorize `_embedding` space — a table without a `vectorize` field rejects `text`; `vector` (raw numbers) searches any `vector` column, and only you know which embedding space produced both the stored and the query vectors, so keep them from the same model.
  **Rows whose `vectorize` source is `null`/empty/missing have `_embedding` `null` and are silently excluded from any `search_vector` that searches `_embedding` (a `text` query, or a raw `vector` query with `column` omitted or set to `_embedding`). If recall matters, call `query` with `SELECT COUNT(*) FROM <table_name> WHERE _embedding IS NULL AND (<same filter>)` (substitute the table name; bind the same `args`; drop the `AND (...)` clause when no filter is used) to find unembedded rows eligible for the search; if you compare counts instead, do it against `SELECT COUNT(*) FROM <table_name> WHERE <same filter>` after exhausting all pages with `min_score` unset (omit the WHERE clause when no filter is used).**
- `skipped_vectors` in a `search_vector` response counts stored vectors that were corrupt or dimension-mismatched and could not be scored; **it does not count rows with a `null`/empty/missing `vectorize` source — those rows are silently excluded and will not raise `skipped_vectors`.**

## Agent-critical caveats

### Quoting and placeholders

- `query` only accepts read-only `SELECT`/`WITH` statements. Bind all values with `?` and pass them in `args`. Identifiers and table names cannot be bound with `?`; write them directly from `list_tables`/`describe_table` and never let untrusted input choose them. `query` only checks that the statement is read-only, not that the identifiers are safe.
- SQL string literals use single quotes (`'value'`), escaped by doubling (`'can''t'`). Prefer `?`.
- Double quotes are for SQL identifiers, not string values.
- `search_fulltext` takes a raw FTS5 `MATCH` expression in `query`; it is **not** SQL, so do not wrap the whole expression in single quotes.

### Full-text (FTS5) search syntax

Dolmen indexes `fulltext` fields with SQLite FTS5 using the default `unicode61` tokenizer:
case-insensitive, diacritic-insensitive for most Latin characters (some non-Latin or multi-diacritic
characters may not normalize), no stemming. Most punctuation (including hyphens) is a token boundary.

**CJK limitation:** `unicode61` does not segment CJK text — it breaks tokens only at whitespace and
punctuation. Chinese/Japanese are usually written without spaces, so an uninterrupted run of CJK
characters is indexed as one opaque token: a `search_fulltext` term for a word or substring *inside*
the run silently matches nothing (no error; the rows are stored and `LIKE`-queryable via `query`).
Whole-run terms, prefix queries (`中华*`), and space-delimited Korean still tokenize and match. For
keyword recall over space-less CJK text, fall back to vector search over an embedding column
(`vectorize: true` + `search_vector(text=...)`) or `query` with `LIKE`.

- `payment` — one token.
- `payment gateway` — implicit `AND`.
- `payment OR gateway`.
- `payment NOT gateway`.
- `title:payment` — only in the `title` fulltext field.
- `{title body}:payment` — any of those fields.
- `"foo bar"` — phrase (adjacent tokens). Phrases match token adjacency, not literal punctuation.
- `"foo-bar"` — double-quote terms that contain spaces or punctuation; bare `foo-bar` is parsed as multiple terms and usually errors.
- `pay*` — prefix match.
- `NEAR(payment refund)` — proximity search (default near span). Use the group form
  `NEAR(term1 term2 ...)`; `term1 NEAR(term2)` is parsed as an implicit `AND` and does not enforce proximity.
- Terms like `"can't"` must be in double quotes; bare single quotes are a syntax error.

Results are ordered by FTS5 `rank` (BM25 by default): more relevant rows have a lower — more negative — value and are returned first. The rank value itself is not returned.

### Vectors and semantic recall

- `vector` fields accept JSON number arrays of the declared `dim`; stored as float32 blobs, returned as `[]float64`.
- `vectorize: true` on a string/text field stores one embedding per non-empty row in `_embedding`. Only one field per table can be vectorized.
- **Vector search has silent recall holes.** Rows with `null`, empty string, or missing values in the vectorized field have `_embedding` NULL and are silently excluded from any `search_vector` that searches `_embedding` — whether it is a `text` query or a raw `vector` query with `column` omitted or set to `_embedding`. `skipped_vectors` does NOT count those rows — it only counts stored vectors that are corrupt or dimension-mismatched and could not be scored. If recall matters, call `query` with `SELECT COUNT(*) FROM <table_name> WHERE _embedding IS NULL AND (<same filter>)` (substitute the table name; bind the same `args`; drop the `AND (...)` clause when no filter is used) to find unembedded rows eligible for the search. If you instead compare counts, compare against `SELECT COUNT(*) FROM <table_name> WHERE <same filter>` after exhausting all pages with `min_score` unset (omit the WHERE clause when no filter is used) — otherwise pagination, `min_score`, offsets, filters, and response truncation can make embedded rows look missing.
- `search_vector(text=...)` embeds the query `text` with the configured provider and searches only the vectorize `_embedding` space — a table without a `vectorize` field rejects `text`. `search_vector(vector=[...])` supplies a query vector directly and may search any vector column.
- `column` applies to `vector` queries: it names the stored-vectors column and defaults to `_embedding` (if a vectorized field exists) or the first declared `vector` field. The query and stored vectors must come from the same embedding space. For `_embedding` (from `vectorize`) this means the same provider/identity; for caller-supplied `vector` fields it means the same model used for the stored and query vectors.
- Each result has `_score`: cosine similarity, higher is closer, typically `0`–`1` for positive embeddings (mathematically `-1`–`1`).
- `_embedding` is hidden from `SELECT *` and search results unless referenced explicitly or `include_hidden: true`.

### Id, `created_at`, and stability

- Every row has `id` and `created_at`. You cannot supply them; they are assigned on insert and returned in reads.
- `id` is `AUTOINCREMENT` — monotonically increasing and never reused after deletes — so it is safe to key off across sessions.
- `created_at` is a UTC millisecond ISO string, e.g. `2026-09-03T12:34:56.123Z`. Use string comparisons or SQLite date/time functions.

### Limits and guardrails

| Resource | Limit | Behavior |
|---|---|---|
| Namespace name | `^[a-z0-9][a-z0-9_-]{0,63}$` (max 64 chars) | rejected |
| Table / field name | `^[a-z][a-z0-9_]{0,63}$` (max 64 chars); reserved names (`id`, `created_at`, `_embedding`, `_score`, `_rank`, `rowid`) are rejected, and a field named `rank` is rejected when `fulltext: true` (reserved by the FTS5 index); table also cannot contain `__fts` or start with `sqlite_` | rejected |
| Table fields | 100 user-defined fields (not counting the implicit `id`, `created_at`, `_embedding` columns) | rejected |
| Records per `insert` | 1,000 | rejected |
| Idempotency key length | 1–256 bytes; use printable ASCII; omit the field for a non-idempotent insert | empty and over-256-byte keys are rejected; the JSON Schema enforces non-empty printable ASCII for schema-validating clients |
| Vector dimension (declared `vector` fields) | 1–4096 | rejected |
| Search `limit` | default 10, max 200 | omit `limit` for the default of 10; the tool schema enforces 1–200 for schema-validating clients, and the server clamps values above 200 to 200 (0 or negative selects the default on direct `/v1` calls) |
| `query` result rows | 1,000 | truncated with `truncated: true` |
| `query` / search result size | 32 MiB | first row over budget errors; later rows truncate; a single BLOB value over 32 MiB always errors |
| Request body | 32 MiB | rejected |
| `query` `args` | 100 | rejected |

Vector search is brute-force (fine into the low millions of rows); FTS5 uses an inverted index and is much faster.

Validation notes:

- `number` becomes `int64` or `float64`: integral values within the int64 range become `int64`; unsigned Go values > `MaxInt64` are rejected, and integral JSON numbers outside the int64 range become `float64` (precision loss).
- `timestamp` must be a parseable ISO/RFC3339 string.
- `vector` must be a number array of exactly the declared `dim`; `NaN`/`Inf` are rejected. The 4096-dimension cap applies only to declared `vector` fields; `vectorize` records the provider's returned dimension.
- Unknown field keys are rejected. Missing or `null` required fields are rejected on `insert`. Fields
  may carry a declared `default` (shown by `describe_table`): an insert omitting such a field stores
  the default instead of NULL; an explicit `null` still stores NULL.
- Namespace and table names are trimmed and lowercased before validation on direct `/v1` requests, so `"namespace":" Production "` operates on `production`. The MCP tool schemas require already-canonical names — always send trimmed lowercase names.
- `query` accepts only `SELECT`/`WITH`, rejects embedded semicolons (trailing semicolons are accepted), and binds at most 100 `args`.
- `search_vector` with `text` requires a provider and searches only the server-managed `_embedding` column produced by a `vectorize: true` field — the provider identity must match the one that embedded the table, and a `text` query naming a declared `vector` column is rejected. Searches with a caller-supplied `vector` need no provider and are not checked against any embedding space — only you know which model produced the stored and query vectors.
- `insert` with an `idempotency_key`: the same key + same records replays the original ids; the same key with different records is rejected. Use printable ASCII keys (`[ -~]`) up to 256 bytes.

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
