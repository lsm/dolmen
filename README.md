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

```bash
CGO_ENABLED=0 go build -o dolmen .
./dolmen -addr 127.0.0.1:8790 -data ./data
```

Optional embedding provider (enables `vectorize` fields and text queries in `search_vector`;
any OpenAI-compatible endpoint works — OpenAI, Ollama, vLLM):

```bash
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

### HTTP API

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

curl -s localhost:8790/v1/insert -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "table": "events",
  "records": [{"title": "first bug", "detail": "token expiry not checked", "score": 0.9, "embedding": [0.1, 0.2, 0.3, 0.4]}]
}'

curl -s localhost:8790/v1/search_fulltext -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "table": "events", "query": "bug"
}'

# raw-vector search on the caller-supplied embedding column (no provider needed;
# "text" queries instead require a vectorize field plus a provider — see below)
curl -s localhost:8790/v1/search_vector -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "table": "events", "vector": [0.1, 0.2, 0.3, 0.4]
}'

curl -s localhost:8790/v1/query -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "sql": "SELECT title, score FROM events WHERE score > ?", "args": [0.5]
}'

# update rows matching a WHERE filter (upsert inserts when nothing matches)
curl -s localhost:8790/v1/update -H 'Content-Type: application/json' -d '{
  "namespace": "myapp", "table": "events", "filter": "score < ?", "args": [0.5],
  "set": {"score": 0.5, "title": "triaged bug"}
}'
```

### MCP (agents)

```bash
claude mcp add --transport http dolmen http://127.0.0.1:8790/mcp
```

The MCP server exposes the same thirteen operations as tools (`tools/list` shows them with full schemas).

## Tools

| Tool | Purpose |
|---|---|
| `list_tables` | Tables in a namespace |
| `describe_table` | Schema, version, row count |
| `create_table` | Typed fields with `fulltext` / `vector` / `vectorize` annotations |
| `infer_schema` | Propose fields from sample records (creates nothing) |
| `insert` | Validated records; indexes and embeddings update automatically; `idempotency_key` makes retries replay the original ids |
| `upsert_by_key` | Insert-or-update keyed by natural field(s) (`on`); converges instead of duplicating on retry |
| `query` | Read-only SQL (SELECT/WITH), parameter binding via `args`, typed results |
| `search_fulltext` | FTS5 MATCH over `fulltext` fields, relevance-ordered, typed results |
| `search_vector` | Cosine KNN over embeddings; pass `text` (server embeds) or `vector`; results carry `_score` |
| `delete` | WHERE-filtered delete, cascades to search indexes |
| `update` | WHERE-filtered field update; reindexes full-text rows and re-embeds changed vectorized fields |
| `upsert` | Update matching rows, or insert one record when the filter matches nothing |
| `migrate` | `add_field`, `rename_field`, `drop_field`, `set_fulltext`, `set_vectorize`; versioned + logged |

## Model

- **Namespace = one SQLite file** (`data/<ns>.db`, WAL). Isolation is physical; drop a namespace by
  shutting the server down cleanly first, then deleting the file (the server caches open connections
  and WAL sidecars, so deleting under a live server is unreliable). A small registry inside each file
  holds table schemas, versions, and a migration log.
- **Full-text** via SQLite FTS5 shadow tables, maintained on insert/update/delete/migrate.
- **Idempotent writes** for agent retries: `insert` accepts an `idempotency_key` (client-chosen,
  durably recorded with its ids in a side table, so a retry — even after a restart — returns the
  original ids; reusing a key for different records is an error), and `upsert_by_key` writes
  records keyed by a natural field set (`on`), updating the matched row partially or inserting
  when nothing matches.
- **Vectors** stored as float32 blobs; KNN is a brute-force cosine scan in Go — fine into the low
  millions of rows, zero index infrastructure. (This is the deliberate MVP trade.)
- **Read-only SQL** runs on a `mode=ro` connection with a SELECT/WITH allowlist — defense in depth.
- **Typed reads** across `query`, `search_fulltext`, and `search_vector`: results honor declared field
  types — `boolean` → `true`/`false`, `json` → the decoded value, `vector` → a number array, `number` →
  integer or float, SQL `NULL` → `null`. In raw SQL, coercion is by result-column label (aliases count
  as their label); labels that match no declared field, or that different tables declare with
  conflicting types, fall back to raw values (blobs as base64). The hidden `_embedding` column (from
  `vectorize`) is stripped from `SELECT *` and search results — reference it in the SQL (outside string
  literals and comments) or pass `include_hidden: true` to a search to include it.
- **Embeddings** are pluggable: `none` (caller supplies vectors) or any OpenAI-compatible endpoint.
- Namespaces are created implicitly on first use (one file per name); tables are not — call
  `create_table` before inserting. No other management surface to operate.

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
- `search_vector` with `text` embeds the query `text` with the configured provider and compares it
  against the resolved `column`. With `vector` you supply the query vector directly.
- `column` is optional and defaults to `_embedding` if a vectorized field exists, otherwise the first
  declared `vector` field. The query and stored vectors must come from the same embedding space. For
  `_embedding` (from `vectorize`) this means the same provider/identity; for caller-supplied `vector`
  fields it means the same model used to produce the stored and query vectors.
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

### Limits and performance

- `query` returns at most 1000 rows and 32 MiB; `truncated: true` when more rows exist.
- `search_fulltext` and `search_vector` default to 10 results, max 200, and share the 32 MiB budget.
- `insert` accepts up to 1000 records per call; chunk larger batches.
- `create_table` allows up to 100 user fields per table.
- Vector search is a brute-force cosine scan in Go: fine into the low millions of rows, but it has no
  approximate index. FTS5 uses an inverted index and is much faster.

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
