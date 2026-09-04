# dolmen

A single-binary data layer for AI agents. Structured tables, full-text search, and vector search —
over your own storage, with schema management and migration built in.

A dolmen is an ancient stone *table*: durable, zero-maintenance, still standing after everyone who
built it moved on. That's the operating model — one static Go binary, SQLite files on a volume,
no database server to babysit.

## Why

Building an AI skill or agent is fast now; holding its data is not. Any agent can spin up a local
SQLite in a folder, but the moment you need **centralized, multi-user storage** — telemetry, memory,
user metrics, blobs — you cross into a different profession: deploying, operating, backing up, and
migrating a database service. Dolmen removes that burden: point it at a directory, get governed
tables plus search plus an agent-native interface.

## Quickstart

### Prerequisites

- [Go 1.26.5+](https://go.dev/dl/) (only to build; the binary is otherwise standalone).
- Git.
- Optional, for `vectorize` fields and text queries in `search_vector`: nothing — the built-in
  `local` provider embeds in-process (downloads a model on first use) — or an OpenAI-compatible
  embedding endpoint (OpenAI, Ollama, vLLM, or any compatible local server).

### Build and run

```bash
git clone https://github.com/lsm/dolmen.git
cd dolmen
CGO_ENABLED=0 go build -o dolmen .
./dolmen -addr 127.0.0.1:8790 -data ./data
```

On Windows (PowerShell):

```powershell
git clone https://github.com/lsm/dolmen.git
cd dolmen
$env:CGO_ENABLED = 0
go build -o dolmen.exe .
.\dolmen.exe -addr 127.0.0.1:8790 -data ./data
```

The first run creates the data directory (`./data` by default). On Unix it is opened with
owner-only permissions (`0700` for the directory, `0600` for files); on Windows the permission bits
only toggle the read-only attribute, so use NTFS ACLs for owner-only isolation. By default the server
binds to `127.0.0.1:8790` and does **not** authenticate, so keep it on a private interface.

### Health check

Bash:

```bash
curl -s http://127.0.0.1:8790/healthz
```

Windows PowerShell:

```powershell
curl.exe -s http://127.0.0.1:8790/healthz
```

Expected output:

```json
{"status":"ok"}
```

All successful `/v1/{operation}` POSTs return `{"ok":true,"data":{...}}`; errors return
`{"ok":false,"error":{"code":"...","message":"..."}}` — plus `request_id` inside `error` when the
request carried an `X-Request-Id` — with a matching 4xx/5xx status. Error codes are stable for
branching: `invalid_request`, `not_found`, `query_error`, `conflict`, `forbidden`, `internal_error`.
The one exception is `GET /v1/openapi.json`, which serves the raw OpenAPI document.
`/healthz` returns `{"status":"ok"}` and `/mcp` returns JSON-RPC responses.

### First API calls

These examples are shown with Bash `curl`. Windows PowerShell variants follow each command; use
`curl.exe` (PowerShell's `curl` is an alias for `Invoke-WebRequest`), double quotes around the `-H`
value, and single quotes around the one-line JSON payload with each inner `"` escaped as `\"` —
Windows PowerShell 5.1 strips unescaped inner quotes when invoking native commands, which corrupts
the JSON. Expected outputs are identical.

Create a table:

```bash
curl -s localhost:8790/v1/create_table -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "table": "events",
  "fields": [
    {"name": "title", "type": "string", "fulltext": true},
    {"name": "detail", "type": "text"},
    {"name": "score", "type": "number"},
    {"name": "embedding", "type": "vector", "dim": 4}
  ]
}'
```

Windows PowerShell:

```powershell
curl.exe -s http://127.0.0.1:8790/v1/create_table -H "Content-Type: application/json" -d '{\"namespace\":\"myapp\",\"table\":\"events\",\"fields\":[{\"name\":\"title\",\"type\":\"string\",\"fulltext\":true},{\"name\":\"detail\",\"type\":\"text\"},{\"name\":\"score\",\"type\":\"number\"},{\"name\":\"embedding\",\"type\":\"vector\",\"dim\":4}]}'
```

Expected output (schema summary, `version` starts at `1`):

```json
{"ok":true,"data":{"table":{"namespace":"myapp","name":"events","version":1,"fields":[{"name":"title","type":"string","fulltext":true},{"name":"detail","type":"text"},{"name":"score","type":"number"},{"name":"embedding","type":"vector","dim":4}]}}}
```

Insert a record:

```bash
curl -s localhost:8790/v1/insert -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "table": "events",
  "records": [{"title": "first bug", "detail": "token expiry not checked", "score": 0.75, "embedding": [0.5, 0.25, -0.5, 0.0]}]
}'
```

Windows PowerShell:

```powershell
curl.exe -s http://127.0.0.1:8790/v1/insert -H "Content-Type: application/json" -d '{\"namespace\":\"myapp\",\"table\":\"events\",\"records\":[{\"title\":\"first bug\",\"detail\":\"token expiry not checked\",\"score\":0.75,\"embedding\":[0.5,0.25,-0.5,0.0]}]}'
```

Expected output:

```json
{"ok":true,"data":{"ids":[1],"inserted":1}}
```

Search full text:

```bash
curl -s localhost:8790/v1/search_fulltext -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "table": "events", "query": "bug"
}'
```

Windows PowerShell:

```powershell
curl.exe -s http://127.0.0.1:8790/v1/search_fulltext -H "Content-Type: application/json" -d '{\"namespace\":\"myapp\",\"table\":\"events\",\"query\":\"bug\"}'
```

Expected output:

```json
{"ok":true,"data":{"results":[{"id":1,"created_at":"...","title":"first bug","detail":"token expiry not checked","score":0.75,"embedding":[0.5,0.25,-0.5,0.0]}],"truncated":false}}
```

Raw vector search on a caller-supplied embedding column (no provider needed; `text` queries instead
require a `vectorize` field plus a provider):

```bash
curl -s localhost:8790/v1/search_vector -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "table": "events", "vector": [0.5, 0.25, -0.5, 0.0]
}'
```

Windows PowerShell:

```powershell
curl.exe -s http://127.0.0.1:8790/v1/search_vector -H "Content-Type: application/json" -d '{\"namespace\":\"myapp\",\"table\":\"events\",\"vector\":[0.5,0.25,-0.5,0.0]}'
```

Run read-only SQL:

```bash
curl -s localhost:8790/v1/query -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "sql": "SELECT title, score FROM events WHERE score > ?", "args": [0.5]
}'
```

Windows PowerShell:

```powershell
curl.exe -s http://127.0.0.1:8790/v1/query -H "Content-Type: application/json" -d '{\"namespace\":\"myapp\",\"sql\":\"SELECT title, score FROM events WHERE score > ?\",\"args\":[0.5]}'
```

Update rows matching a WHERE filter (and `upsert` inserts when nothing matches):

```bash
curl -s localhost:8790/v1/update -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "table": "events", "filter": "score > ?", "args": [0.5],
  "set": {"score": 0.5, "title": "triaged bug"}
}'
```

Windows PowerShell:

```powershell
curl.exe -s http://127.0.0.1:8790/v1/update -H "Content-Type: application/json" -d '{\"namespace\":\"myapp\",\"table\":\"events\",\"filter\":\"score > ?\",\"args\":[0.5],\"set\":{\"score\":0.5,\"title\":\"triaged bug\"}}'
```

### Optional: embeddings

Bash:

```bash
# Built-in local embeddings — in-process inference, zero external services.
# Downloads sentence-transformers/all-MiniLM-L6-v2 (~90 MB) from the Hugging
# Face Hub on first use and caches it under the data dir; every later start
# reuses the cache.
DOLMEN_EMBED_PROVIDER=local ./dolmen

# Another model (e.g. multilingual/CJK — see the full-text CJK caveat):
DOLMEN_EMBED_PROVIDER=local \
DOLMEN_EMBED_MODEL=sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2 \
./dolmen

# An OpenAI-compatible endpoint instead (OpenAI, Ollama, vLLM):
DOLMEN_EMBED_PROVIDER=openai \
DOLMEN_EMBED_API_KEY=sk-... \
DOLMEN_EMBED_MODEL=text-embedding-3-small \
./dolmen

# OpenAI-compatible local endpoints (Ollama, vLLM) need the base URL:
DOLMEN_EMBED_PROVIDER=openai \
DOLMEN_EMBED_BASE_URL=http://localhost:11434/v1 \
DOLMEN_EMBED_MODEL=nomic-embed-text \
./dolmen
```

Windows PowerShell:

```powershell
$env:DOLMEN_EMBED_PROVIDER = "local"
.\dolmen.exe

$env:DOLMEN_EMBED_PROVIDER = "openai"
$env:DOLMEN_EMBED_API_KEY = "sk-..."
$env:DOLMEN_EMBED_MODEL = "text-embedding-3-small"
.\dolmen.exe

# OpenAI-compatible local endpoints (Ollama, vLLM):
$env:DOLMEN_EMBED_PROVIDER = "openai"
$env:DOLMEN_EMBED_BASE_URL = "http://localhost:11434/v1"
$env:DOLMEN_EMBED_MODEL = "nomic-embed-text"
.\dolmen.exe
```

Local provider notes:

- **Model cache** lives at `<data>/models/` (one `org--name` directory per model). Set
  `REMBED_CACHE` to override the location, and `HF_TOKEN` for gated Hugging Face repos.
- **Pick a symmetric model.** Dolmen embeds stored rows and query text through the same call,
  so models whose retrieval contract needs role prefixes — the e5 family (`query: `/`passage: `),
  bge, arctic — silently rank worse than they should. The default MiniLM and the
  `sentence-transformers/paraphrase-multilingual-*` models are symmetric and safe.
- **Offline installs**: pre-seed the cache by copying `<data>/models/org--name/` from a machine
  that already downloaded the model, or set `DOLMEN_EMBED_MODEL` to an absolute model-directory
  path (the same directory layout) to skip the Hub entirely. Note: for bert-family models
  (including the MiniLM default) a pre-seeded Hub-id model still makes one small Hub request per
  process start to check for optional tokenizer files; the directory form (and the
  xlm-roberta, modernbert, and gemma model families) makes none.
- **Identity pinning** works as with the OpenAI provider: tables record `local/<model>` as their
  embedding space, and a model change is rejected until the table is re-embedded
  (`migrate` with `set_vectorize` off, then on).

## Configuration

Dolmen reads its startup configuration from command-line flags and environment
variables. Unknown flags and positional arguments are rejected with an error.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `-addr` | `DOLMEN_ADDR` | `127.0.0.1:8790` | HTTP/MCP listen address |
| `-data` | `DOLMEN_DATA` | `data` | Data directory (one SQLite file per namespace) |
| `-version` | — | — | Print version and exit |
| — | `DOLMEN_ALLOWED_ORIGINS` | — | Comma-separated allowed HTTP origins for CORS; `localhost`, `127.0.0.1`, and `::1` are always allowed |
| — | `DOLMEN_EMBED_PROVIDER` | `none` | Embedding provider: `none` (caller supplies vectors), `local` (built-in in-process embeddings via [rembed](https://github.com/rostamlabs/rembed)), or `openai` (any OpenAI-compatible endpoint). Unknown values produce an error |
| — | `DOLMEN_EMBED_BASE_URL` | `https://api.openai.com/v1` | Base URL for an OpenAI-compatible provider |
| — | `DOLMEN_EMBED_MODEL` | provider default | Model: `sentence-transformers/all-MiniLM-L6-v2` for `local` (or an absolute model-directory path), `text-embedding-3-small` for `openai` |
| — | `DOLMEN_EMBED_API_KEY` | — | API key for an OpenAI-compatible provider. If set (even to `""`), it takes precedence over `OPENAI_API_KEY` |
| — | `OPENAI_API_KEY` | — | Fallback API key when `DOLMEN_EMBED_API_KEY` is unset |
| — | `REMBED_CACHE` | `<data>/models` | Model cache directory for the `local` provider (overrides the data-dir location) |
| — | `HF_TOKEN` | — | Hugging Face token for gated repos downloaded by the `local` provider |

## MCP (agents)

```bash
claude mcp add --transport http dolmen http://127.0.0.1:8790/mcp
```

The MCP server exposes the same eighteen operations as tools (`tools/list` shows them with input/output schemas and annotations). Successful `tools/call` results carry `structuredContent` — the result as a JSON object matching the tool's `outputSchema` — with no text mirror (`content` stays an empty array: the spec keeps it mandatory); tool errors are reported as text with `isError: true`.

Skill distribution is built into the server. `GET /skills` returns a JSON manifest with links to the layered skill markdown; `GET /skills/dolmen` is the end-user skill and `GET /skills/dolmen-admin` is the developer skill. Agents should fetch the skill from the running binary instead of copying a static file.

## Tools

| Tool | Purpose |
|---|---|
| `list_namespaces` | Namespaces on this server |
| `create_namespace` | Reserve a namespace up front (creation is implicit on first use otherwise) |
| `drop_namespace` | Delete a namespace and all its tables; `confirm` must repeat the name |
| `list_tables` | Tables in a namespace |
| `describe_table` | Schema, version, row count |
| `create_table` | Typed fields with `fulltext` / `vector` / `vectorize` / `default` annotations (`default` is stored by inserts that omit the field) |
| `infer_schema` | Propose fields from sample records (creates nothing) |
| `insert` | Validated records; indexes and embeddings update automatically; `idempotency_key` makes retries replay the original ids |
| `upsert_by_key` | Insert-or-update keyed by natural field(s) (`on`); converges instead of duplicating on retry |
| `query` | Read-only SQL (SELECT/WITH), parameter binding via `args`, typed results |
| `search_fulltext` | FTS5 MATCH over `fulltext` fields, relevance-ordered, typed results |
| `search_vector` | Cosine KNN; `text` (server embeds; searches only the vectorize `_embedding` space) or raw `vector` (any vector column, caller owns the space); results carry `_score` and `skipped_vectors` |
| `delete` | WHERE-filtered delete, cascades to search indexes |
| `drop_table` | Drop a table — rows, search index, schema, history, idempotency keys; `confirm` must repeat the name |
| `update` | WHERE-filtered field update; reindexes full-text rows and re-embeds changed vectorized fields |
| `upsert` | Update matching rows, or insert one record when the filter matches nothing |
| `migrate` | `add_field` (optional `default` backfills existing rows — required fields land on populated tables as `NOT NULL DEFAULT`; optional fields get a one-time backfill, later omitted inserts store NULL), `rename_field`, `drop_field`, `set_fulltext`, `set_vectorize`; `expected_version` asserts the schema being migrated (required for rename/drop, conflicts surface as 409), `dry_run` previews the plan without side effects; versioned + logged |
| `list_migrations` | A table's migration history, newest first, with the exact recorded changes |

## Model

- **Namespace = one SQLite file** (`data/<ns>.db`, WAL). Isolation is physical. Lifecycle is managed
  over the API: `list_namespaces`, `create_namespace`, and `drop_namespace` (which closes the server's
  own connections, then deletes the file and its WAL sidecars — `confirm` must repeat the namespace
  name, and any later use of the name recreates the namespace empty). Safety caveat: drop coordinates
  only within one server — another process holding the file open (a second dolmen instance, a backup
  tool) is not detected, and racing in-flight requests on the namespace may fail, so quiesce writers
  before dropping. A small registry inside each file holds table schemas, versions, and a migration
  log (surfaced by `list_migrations`).
- **Full-text** via SQLite FTS5 shadow tables, maintained on insert/update/delete/migrate.
- **Idempotent writes** for agent retries: `insert` accepts an `idempotency_key` (client-chosen,
  durably recorded with its ids in a side table, so a retry — even after a restart — returns the
  original ids; reusing a key for different records is an error), and `upsert_by_key` writes
  records keyed by a natural field set (`on`), updating the matched row partially or inserting
  when nothing matches.
- **Vectors** stored as float32 blobs; KNN is a brute-force cosine scan in Go — fine into the low
  millions of rows, zero index infrastructure. (This is the deliberate MVP trade.)
- **Vector-search spaces** are kept honest: `text` queries are embedded by the active provider and
  only search the server-managed `vectorize` (`_embedding`) space, whose model identity is pinned
  per table — a provider change is rejected until the table is re-embedded. Caller-provided `vector`
  columns are searchable only with a raw `vector` query, because only the caller knows which embedding
  space produced them. Stored vectors that are corrupt, dimension-mismatched, or non-finite are
  skipped from scoring and reported as `skipped_vectors`, so a search never silently drops rows.
- **Read-only SQL** runs on a `mode=ro` connection with a SELECT/WITH allowlist — defense in depth.
- **Typed reads** across `query`, `search_fulltext`, and `search_vector`: results honor declared field
  types — `boolean` → `true`/`false`, `json` → the decoded value, `vector` → a number array, `number` →
  integer or float, SQL `NULL` → `null`. In raw SQL, coercion is by result-column label (aliases count
  as their label); labels that match no declared field, or that different tables declare with
  conflicting types, fall back to raw values (blobs as base64). The hidden `_embedding` column (from
  `vectorize`) is stripped from `SELECT *` and search results — reference it in the SQL (outside string
  literals and comments) or pass `include_hidden: true` to a search to include it.
- **Pagination** on `query`, `search_fulltext`, and `search_vector` via `offset` and `limit` parameters.
  Do not put `LIMIT`/`OFFSET` in raw SQL; use the parameters. `search_fulltext` and `search_vector`
  have stable, deterministic ordering. The response includes `truncated: true` when more results are
  available beyond the returned page.
- **Embeddings** are pluggable: `none` (caller supplies vectors), `local` (built-in in-process
  inference via [rembed](https://github.com/rostamlabs/rembed) — pure Go, no cgo, model weights
  cached under the data dir), or any OpenAI-compatible endpoint.
- Namespaces are created implicitly on first use (one file per name; `create_namespace` just reserves
  the name up front); tables are not — call `create_table` before inserting, `drop_table` (confirm-guarded)
  to remove one completely. No other management surface to operate.

Storage sits behind the store layer, so engines like DuckDB-over-Parquet or Iceberg-over-S3 can be
added as adapters without touching the API or MCP surface.

## Query and search notes

### SQL `query` quoting

- `query` is read-only: only `SELECT` or `WITH` statements are allowed, and only one statement at a time.
- Bind all **values** with `?` placeholders and the `args` array. Identifiers and table names cannot be
  bound with `?`, so write them directly from `list_tables`/`describe_table` and treat them as an
  allowlist, not user input. `query` only checks that the statement is read-only, not that the
  identifiers are safe.
- SQL string literals use single quotes (`'value'`). Escape a single quote by doubling it
  (`'can''t'`), or better, use a `?` placeholder.
- Double quotes are for SQL identifiers, not string values.
- `id` and `created_at` are real columns: you can filter, order, and select them.

### Full-text search (FTS5)

Fields marked `fulltext: true` are indexed with a shadow SQLite FTS5 table. The default tokenizer is
`unicode61`: case-insensitive, diacritic-insensitive for most Latin characters (some non-Latin or
multi-diacritic characters may not normalize), and it does **not** stem. Most punctuation, including
hyphens, is a token boundary.

`search_fulltext` takes a raw FTS5 `MATCH` expression in `query`. It is **not** SQL, so do not wrap
the whole expression in single quotes.

Common syntax:

- `payment` — a single token.
- `payment gateway` — implicit `AND` between tokens.
- `payment OR gateway` — either token.
- `payment NOT gateway` — must contain `payment` and must not contain `gateway`.
- `title:payment` — only in the `title` fulltext field.
- `{title body}:payment` — in any of the named fulltext fields.
- `"foo bar"` — phrase (adjacent tokens). Because stored punctuation is also tokenized, a phrase
  matches token adjacency, not literal punctuation.
- `"foo-bar"` — double-quote any term that contains spaces or punctuation (hyphens, dots, slashes,
  apostrophes). Bare `foo-bar` is parsed as multiple terms and usually errors.
- `pay*` — prefix match.
- `NEAR(payment refund)` — proximity search (default near span). The group form
  `NEAR(term1 term2 ...)` enforces proximity; writing `term1 NEAR(term2)` instead parses as an
  implicit `AND` and does **not** enforce proximity.

Terms containing an apostrophe, such as `"can't"`, must be inside a double-quoted phrase. Bare
single quotes in an FTS5 query are a syntax error.

Results are ordered by FTS5 `rank` (BM25 by default). More relevant documents have a lower — more
negative — `rank` value and are returned first. The rank value itself is not included in results.

### Vectors and semantic search

- `vector` fields store caller-supplied float arrays. Pass them as JSON number arrays; the server
  stores them as float32 blobs and returns them as `[]float64` in reads.
- `vectorize: true` on one string/text field makes the server embed that field into the hidden
  `_embedding` column. Only one field per table can be vectorized; only non-empty values are embedded,
  so rows with `null`, empty strings, or missing values have `_embedding` NULL and are excluded from
  vector search.
- `search_vector` with `text` embeds the query `text` with the configured provider and searches only
  the vectorize `_embedding` space — a table without a `vectorize` field rejects `text`. With
  `vector` you supply the query vector directly and may search any vector column.
- `column` is optional for `vector` queries: it names the stored-vectors column and defaults to
  `_embedding` if a vectorized field exists, otherwise the first declared `vector` field. The query
  and stored vectors must come from the same embedding space. For `_embedding` (from `vectorize`)
  this means the same provider/identity; for caller-supplied `vector` fields it means the same model
  used to produce the stored and query vectors.
- Every vector result carries `_score`: cosine similarity, where higher is closer. For typical
  positive embeddings it ranges `0`–`1`; mathematically it ranges `-1`–`1`.
- `_embedding` is hidden from `SELECT *` and search results unless you reference it explicitly in the
  SQL or pass `include_hidden: true`.

### Id, `created_at`, and stability

Every row has two implicit columns:

- `id` — `INTEGER PRIMARY KEY AUTOINCREMENT`. It is assigned on insert, cannot be supplied or set,
  and is never reused after rows are deleted. Ids are safe to reference across sessions.
- `created_at` — a UTC millisecond timestamp in ISO/RFC3339 form, e.g. `2026-09-03T12:34:56.123Z`.
  It is set on insert and cannot be supplied or set. Use string comparisons or SQLite date/time
  functions on it.

`SELECT *` includes both columns.

## Limits and guardrails

| Resource | Limit | Behavior when exceeded |
|---|---|---|
| Namespace name | `^[a-z0-9][a-z0-9_-]{0,63}$` (max 64 chars) | rejected |
| Table / field name | `^[a-z][a-z0-9_]{0,63}$` (max 64 chars); reserved names (`id`, `created_at`, `_embedding`, `_score`, `_rank`, `rowid`) are rejected, and a field named `rank` is rejected when `fulltext: true` (reserved by the FTS5 index); table also cannot contain `__fts` or start with `sqlite_` | rejected |
| Table fields | 100 user-defined fields (not counting the implicit `id`, `created_at`, `_embedding` columns) | rejected |
| Records per `insert` / `upsert_by_key` | 1,000 | rejected |
| Natural key fields per `upsert_by_key` | 8 | rejected |
| Idempotency key length | 1–256 bytes; use printable ASCII (`[ -~]`); omit the field for a non-idempotent insert | empty and over-256-byte keys are rejected; the JSON Schema enforces non-empty printable ASCII for schema-validating clients |
| Vector dimension (declared `vector` fields) | 1–4096 | rejected |
| Search `limit` (`search_fulltext`, `search_vector`) | default 10, hard max 200 | omit `limit` for the default of 10; the tool schema enforces 1–200 for schema-validating clients, and the server clamps values above 200 to 200 (0 or negative selects the default on direct `/v1` calls) |
| `query` result rows | 1,000 | truncated; `truncated` is `true` in the response |
| `query` / search result bytes | 32 MiB | first row over budget errors; later rows truncate; a single BLOB value over 32 MiB always errors |
| Request body size | 32 MiB | rejected with `413 Request Entity Too Large` |
| `query` `args` | 100 | rejected |
| `infer_schema` samples | 1–50 | rejected |
| Column label in `query` | 4096 bytes | rejected |

Coercion and validation rules:

- `number`: JSON numbers and Go numeric types become `int64` when integral and within the int64
  range, otherwise `float64`. Unsigned Go integer values larger than `math.MaxInt64` are rejected;
  integral JSON numbers outside the int64 range are stored as `float64` (precision loss).
- `boolean`: stored as `0` or `1`; returned as `true`/`false`.
- `timestamp`: stored as RFC3339/ISO strings with minimal canonicalization (whitespace trimmed,
  lowercase `t`/`z` uppercased; offsets and date-only/space-separated forms are preserved as
  given). Accepted forms include `YYYY-MM-DD`, `YYYY-MM-DD HH:MM:SS`, and
  `YYYY-MM-DDTHH:MM:SS[±HH:MM|Z]`; offsets must be ≤ `±23:59`. Mixed-offset values do not sort
  chronologically as strings — normalize to UTC before storing if you order by this field.
- `json`: stored as JSON text; returned as the decoded value.
- `vector`: stored as a float32 blob. Input must be a number array of exactly the declared dimension;
  `NaN`, `Inf`, and out-of-range values are rejected. The 4096-dimension cap in the table above
  applies only to manually declared `vector` fields; `vectorize: true` records the provider's
  returned dimension.
- `string` / `text`: stored as TEXT.
- Unknown field keys are rejected. Required fields missing or `null` are rejected on `insert` and on
  the insert branch of `upsert`/`upsert_by_key`; `update` and matched `upsert`/`upsert_by_key` accept
  partial `set` maps and only reject setting a required field to `null`.
- `query` only accepts `SELECT` or `WITH` statements, rejects embedded semicolons (no multiple
  statements), and binds at most 100 `args`.
- On direct `/v1` requests, namespace and table names are trimmed and lowercased before validation,
  so `namespace: " Production "` silently operates on `production`. The MCP tool schemas require
  already-canonical names, so schema-validating clients must send trimmed lowercase names.
- `insert` with an `idempotency_key`: the same key and the same records replay the original ids; the
  same key with different records is rejected. Use printable ASCII keys (`[ -~]`) up to 256 bytes.
- `search_vector` with `text` requires a provider and searches only the server-managed `_embedding`
  column produced by a `vectorize: true` field — the provider identity must match the one that
  embedded the table, and a `text` query naming a declared `vector` column is rejected. Searches
  with a caller-supplied `vector` need no provider and are not checked against any embedding
  space — only you know which model produced the stored and query vectors.

## Platform and filesystem support

Dolmen uses [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite), a pure-Go SQLite driver, so
no CGO is required.

| Concern | Policy |
|---|---|
| Operating systems | Linux, macOS, and Windows are supported. |
| Filesystem | Local filesystems (ext4, APFS, NTFS, etc.) are required. SQLite WAL uses shared-memory coordination that does not work reliably over network or shared filesystems (NFS, SMB); these are unsupported and the first namespace open may fail or operate without WAL locking guarantees. |
| WAL | Enabled per namespace (`journal_mode=WAL`, `synchronous=NORMAL`). Expect `<ns>.db`, `<ns>.db-wal`, and `<ns>.db-shm` files. |
| Permissions | On Unix the data directory is created `0700` and namespace `.db`/`-wal`/`-shm` files are set `0600` (owner only); on Windows `os.Chmod` only toggles the read-only attribute, so use NTFS ACLs for owner-only isolation. Permission failures surface when a namespace is first opened, not necessarily at server startup, so `/healthz` can succeed before that point. |
| Locking | Each namespace has one writer connection (`MaxOpenConns=1`) with `BEGIN IMMEDIATE` locking, plus a separate read-only connection pool. WAL mode allows multiple concurrent readers, but only one writer per file at a time. |
| Multi-process | SQLite's file locking makes concurrent processes safe in principle, but running two dolmen servers against the same data directory can cause `database is locked` errors and is not recommended. |
| Deleting a namespace | Prefer `drop_namespace` (confirm-guarded, closes the server's own connections first). Manually: stop the dolmen process, then delete the three `<ns>.db*` files. |

## Not yet (deliberately)

Authn/authz, quotas, multi-node, time travel, compaction, a UI, an Iceberg adapter. The MVP exists
to validate the tool surface with real agents. **Until auth exists, dolmen binds to 127.0.0.1 —
keep it on a private interface.**

## Development

```bash
make test     # go vet + go test ./...
make build    # static binary
make run      # run on :8790 with ./data
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
