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
```

### MCP (agents)

```bash
claude mcp add --transport http dolmen http://127.0.0.1:8790/mcp
```

The MCP server exposes the same ten operations as tools (`tools/list` shows them with full schemas).

## Tools

| Tool | Purpose |
|---|---|
| `list_tables` | Tables in a namespace |
| `describe_table` | Schema, version, row count |
| `create_table` | Typed fields with `fulltext` / `vector` / `vectorize` annotations |
| `infer_schema` | Propose fields from sample records (creates nothing) |
| `insert` | Validated records; indexes and embeddings update automatically |
| `query` | Read-only SQL (SELECT/WITH), parameter binding via `args`, typed results |
| `search_fulltext` | FTS5 MATCH over `fulltext` fields, relevance-ordered, typed results |
| `search_vector` | Cosine KNN over embeddings; pass `text` (server embeds) or `vector`; results carry `_score` |
| `delete` | WHERE-filtered delete, cascades to search indexes |
| `migrate` | `add_field`, `rename_field`, `drop_field`, `set_fulltext`, `set_vectorize`; versioned + logged |

## Model

- **Namespace = one SQLite file** (`data/<ns>.db`, WAL). Isolation is physical; drop a namespace by
  shutting the server down cleanly first, then deleting the file (the server caches open connections
  and WAL sidecars, so deleting under a live server is unreliable). A small registry inside each file
  holds table schemas, versions, and a migration log.
- **Full-text** via SQLite FTS5 shadow tables, maintained on insert/delete/migrate.
- **Vectors** stored as float32 blobs; KNN is a brute-force cosine scan in Go — fine into the low
  millions of rows, zero index infrastructure. (This is the deliberate MVP trade.)
- **Read-only SQL** runs on a `mode=ro` connection with a SELECT/WITH allowlist — defense in depth.
- **Typed reads** across `query`, `search_fulltext`, and `search_vector`: results honor declared field
  types — `boolean` → `true`/`false`, `json` → the decoded value, `vector` → a number array, `number` →
  integer or float, SQL `NULL` → `null`. In raw SQL, coercion is resolved by column name across the
  namespace's schemas; expression columns and names declared with conflicting types fall back to raw
  values (blobs as base64). The hidden `_embedding` column (from `vectorize`) is stripped from
  `SELECT *` and search results — reference it in the SQL or pass `include_hidden: true` to a search
  to include it.
- **Embeddings** are pluggable: `none` (caller supplies vectors) or any OpenAI-compatible endpoint.
- Namespaces are created implicitly on first use (one file per name); tables are not — call
  `create_table` before inserting. No other management surface to operate.

Storage sits behind the store layer, so engines like DuckDB-over-Parquet or Iceberg-over-S3 can be
added as adapters without touching the API or MCP surface.

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
