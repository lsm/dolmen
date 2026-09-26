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

- [Go 1.26.6+](https://go.dev/dl/) (only to build; the binary is otherwise standalone).
- Git.
- For `vectorize` fields and text queries in `search_vector`: nothing extra by default. The built-in
  `local` provider is enabled automatically and embeds in-process (it downloads the model on first
  use). Use an OpenAI-compatible endpoint instead by setting `DOLMEN_EMBED_PROVIDER=openai`.

### Build and run

```bash
git clone https://github.com/lsm/dolmen.git
cd dolmen
CGO_ENABLED=0 go build -o dolmen ./cmd/dolmen
./dolmen -addr 127.0.0.1:8790 -data ./data
```

On Windows (PowerShell):

```powershell
git clone https://github.com/lsm/dolmen.git
cd dolmen
$env:CGO_ENABLED = 0
go build -o dolmen.exe ./cmd/dolmen
.\dolmen.exe -addr 127.0.0.1:8790 -data ./data
```

The first run creates the data directory (`./data` by default) and, with the default `local`
embedding provider, the model cache (`./data/models`). On Unix these are opened with owner-only
permissions (`0700` for the directory, `0600` for files); on Windows the permission bits only toggle
the read-only attribute, so use NTFS ACLs for owner-only isolation. By default the server binds to
`127.0.0.1:8790` and does **not** authenticate, so keep it on a private interface.

> **Embeddings are on by default.** The built-in `local` provider downloads
> `sentence-transformers/all-MiniLM-L6-v2` (~90 MB) from the Hugging Face Hub the first time a
> `vectorize` field is written. Start the server and watch the log for `local embedding model is not
> cached` to confirm the state. Pre-seed the cache, or download the model tarball from the
> [releases page](https://github.com/lsm/dolmen/releases), for offline or HF-blocked installs
> (see [Offline install](#offline-install)).

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
`{"ok":false,"error":{"code":"...","message":"...","request_id":"..."}}` — the request id is the
request's `X-Request-Id` header when one was sent, otherwise a server-generated id, echoed back as
the `X-Request-Id` response header so any error can be correlated with the server log — with a
matching 4xx/5xx status. Error codes are stable for branching: `invalid_request`, `not_found`,
`query_error`, `conflict`, `unauthorized`, `forbidden`, `embedder_unavailable`, `canceled`,
`timeout`, `internal_error`.
`canceled` means the request was cancelled before it completed (over the stdio transport, by a
client cancellation notification or the shutdown drain); the operation may or may not have
finished server-side — check with a query before retrying a write.
`timeout` means the server stopped the request at one of its time limits: `504` when an operation
ran past `-op-timeout` (or `-migrate-timeout`), `408` when the request body arrived too slowly. A
timed-out read can be narrowed and retried; a timed-out write may or may not have committed, so
check before retrying it.
`embedder_unavailable` (503) means the server's embedding provider could not load its model — with
the `local` provider, typically the first-use Hugging Face download failing — and its message names
the offline remediations (pre-seed the model cache, or point `DOLMEN_EMBED_MODEL` at a local model
directory); retrying the same request makes no sense until the model can load.
The one exception is `GET /v1/openapi.json`, which serves the raw OpenAPI document.
`/livez` (and its alias `/healthz`) returns `{"status":"ok"}`, `/readyz` returns `{"status":"ready", ...}`, and `/mcp` returns JSON-RPC responses.

`GET /metrics` serves Prometheus text format: `dolmen_operations_total{op,outcome}` (outcome is `ok` or an error code such as `forbidden` or `timeout`), the `dolmen_operation_duration_seconds{op}` histogram, `dolmen_operations_in_flight`, `dolmen_subscriptions_active`, `dolmen_uptime_seconds` and `dolmen_build_info{version}`. Labels are bounded: only operation names and error codes, never namespaces, tables, principals or request ids, so the series count stays under a few hundred. A useful baseline alert is a rising rate of `outcome="internal_error"` or `outcome="timeout"`, or p99 latency of a hot operation. Correlate a single request through the access log (`-log-level debug`) by its `X-Request-Id`.

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
{"ok":true,"data":{"results":[{"id":1,"created_at":"...","title":"first bug","detail":"token expiry not checked","score":0.75,"embedding":[0.5,0.25,-0.5,0.0]}],"truncated":false,"limit":10}}
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

The `local` provider is enabled by default. To change or disable it, set `DOLMEN_EMBED_PROVIDER`.

Bash:

```bash
# Built-in local embeddings are the default — in-process inference, zero external services.
# It downloads sentence-transformers/all-MiniLM-L6-v2 (~90 MB) from the Hugging
# Face Hub on first use and caches it under the data dir; every later start reuses the cache.
# (To use a different local model, set DOLMEN_EMBED_MODEL.)

# Mixed-language/CJK data — the multilingual model (see "Choosing an
# embedding model" below; dolmen adds the e5 query:/passage: prefixes
# itself, callers do not):
DOLMEN_EMBED_PROVIDER=local \
DOLMEN_EMBED_MODEL=intfloat/multilingual-e5-small \
./dolmen

# Use an OpenAI-compatible endpoint instead (OpenAI, Ollama, vLLM):
DOLMEN_EMBED_PROVIDER=openai \
DOLMEN_EMBED_API_KEY=sk-... \
DOLMEN_EMBED_MODEL=text-embedding-3-small \
./dolmen

# OpenAI-compatible local endpoints (Ollama, vLLM) need the base URL:
DOLMEN_EMBED_PROVIDER=openai \
DOLMEN_EMBED_BASE_URL=http://localhost:11434/v1 \
DOLMEN_EMBED_MODEL=nomic-embed-text \
./dolmen

# Disable server-side embeddings entirely (caller must supply vectors):
DOLMEN_EMBED_PROVIDER=none ./dolmen

# Another local model (e.g. multilingual/CJK — see the full-text CJK caveat):
DOLMEN_EMBED_MODEL=sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2 ./dolmen
```

Windows PowerShell:

```powershell
# OpenAI-compatible endpoint instead of the default local provider:
$env:DOLMEN_EMBED_PROVIDER = "openai"
$env:DOLMEN_EMBED_API_KEY = "sk-..."
$env:DOLMEN_EMBED_MODEL = "text-embedding-3-small"
.\dolmen.exe

# OpenAI-compatible local endpoints (Ollama, vLLM):
$env:DOLMEN_EMBED_PROVIDER = "openai"
$env:DOLMEN_EMBED_BASE_URL = "http://localhost:11434/v1"
$env:DOLMEN_EMBED_MODEL = "nomic-embed-text"
.\dolmen.exe

# Disable server-side embeddings:
$env:DOLMEN_EMBED_PROVIDER = "none"
.\dolmen.exe

# Another local model:
$env:DOLMEN_EMBED_MODEL = "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2"
.\dolmen.exe
```

Whichever provider is configured, `describe_server` reports its status read-only over both `/v1`
and MCP — provider, model, the identity that pins vectorized tables, whether server-side embedding
is usable, and (local only) whether the model is cached (`model_cached`) — so the active provider
is visible without attempting a write. `usable` is
configuration status only: the provider is not called, so an endpoint that is down or rejects its
credentials still fails at first use, not here.

### Choosing an embedding model

`DOLMEN_EMBED_MODEL` accepts any Hugging Face model id the [rembed](https://github.com/rostamlabs/rembed)
runtime supports (or an absolute model-directory path). Two are packaged as release assets and
tested:

- **`sentence-transformers/all-MiniLM-L6-v2`** (default) — English-only, 384-dim, fast, ~90 MB.
  The right choice for English-only data.
- **`intfloat/multilingual-e5-small`** — 100+ languages including Japanese/Chinese/Korean, also
  384-dim, ~450 MB from the Hub (~270 MB as the release tarball). Use it when rows or queries mix
  languages: with the English-centric default, semantic recall is per-language only — an English
  "connection pool exhausted" query will not surface a Japanese 接続プール枯渏 incident.

**The e5 prefix caveat is handled for you.** e5-family models were trained with an asymmetric
retrieval contract — search text prefixed `"query: "` and stored text prefixed `"passage: "` —
and embed exactly the text they are given, so missing prefixes silently degrade ranking. Dolmen
adds the prefixes server-side: stored rows are embedded as `passage: <field value>`, `search_vector`
text queries as `query: <text>`, for any model whose own name segment carries `e5`
(`intfloat/e5-*`, `intfloat/multilingual-e5-*`, and their offline directory forms; instruct-tuned
variants are excluded — they need different prompts). Callers never add or see the prefixes, and
symmetric models — the MiniLM default, the `sentence-transformers/paraphrase-multilingual-*`
family — get nothing prepended. Other asymmetric families (bge, arctic) use longer
model-specific instructions dolmen does not know; avoid them.

**Switching models re-embeds through `migrate`.** Every vectorized table records the identity of
the space it was embedded in — `local/<model>` for symmetric models (e.g.
`local/sentence-transformers/all-MiniLM-L6-v2`), and a versioned marked form when the e5 prefix
contract is active (e.g. `local/v2:intfloat/multilingual-e5-small#e5`) — visible as `identity` in
`describe_server` and `embed_space` in `describe_table`. A server whose identity differs — a
different model, or a model whose prefix contract changed — rejects inserts and text searches on
those tables until each is re-embedded: `migrate` with `set_vectorize` off, then on (the backfill
re-embeds every row).

### Offline install

In networks that block `huggingface.co` (or on air-gapped machines), download a packaged model
instead of relying on the Hub — the English default and the multilingual model are both available
(see "Choosing an embedding model").

Models are not attached to a dolmen release. They change far less often than the binary does, so
they are published once under their own tag and shared by every version; the tag each release uses
is linked from its release notes.

```bash
models="models-v1"
base="https://github.com/lsm/dolmen/releases/download/${models}"
curl -LO "${base}/dolmen-model-all-MiniLM-L6-v2.tar.gz"
# and/or, for mixed-language/CJK data (~270 MB):
curl -LO "${base}/dolmen-model-multilingual-e5-small.tar.gz"

# Option A: extract into the data directory's model cache, then run normally.
# (The multilingual model needs DOLMEN_EMBED_MODEL; the default does not.)
mkdir -p data/models
tar -xzf dolmen-model-multilingual-e5-small.tar.gz -C data/models
DOLMEN_EMBED_PROVIDER=local \
DOLMEN_EMBED_MODEL=intfloat/multilingual-e5-small \
./dolmen

# Option B: extract anywhere and point DOLMEN_EMBED_MODEL at the directory.
mkdir -p /opt/dolmen/models
tar -xzf dolmen-model-multilingual-e5-small.tar.gz -C /opt/dolmen/models
DOLMEN_EMBED_PROVIDER=local \
DOLMEN_EMBED_MODEL=/opt/dolmen/models/intfloat--multilingual-e5-small \
./dolmen
```

Each tarball contains the `org--name` model directory (the same layout the `local` provider uses
under `<data>/models`): ~80 MB compressed for the MiniLM default, ~270 MB for
`multilingual-e5-small`. Each archive opens with a size manifest (`.dolmen-sizes.json`) that lets
the server reject a partially extracted cache instead of loading a truncated model.

Verify the server embeds without reaching `huggingface.co` by creating a vectorized table, inserting
a row, and running a text vector search:

```bash
curl -s localhost:8790/v1/create_table -H 'Content-Type: application/json' -d '{
  "namespace": "demo", "table": "notes",
  "fields": [{"name": "body", "type": "text", "vectorize": true}]
}'
curl -s localhost:8790/v1/insert -H 'Content-Type: application/json' -d '{
  "namespace": "demo", "table": "notes",
  "records": [{"body": "A cat sat on the mat."}]
}'
curl -s localhost:8790/v1/search_vector -H 'Content-Type: application/json' -d '{
  "namespace": "demo", "table": "notes", "text": "feline"
}'
```

Note that `describe_server`'s `usable` is configuration-only — it is true whether or not the cache
actually holds the model — while `model_cached` (local provider) does check the cache on disk:
`true` means the weights are complete and the first vectorized write needs no download; `false`
means that first operation downloads the model from the Hugging Face Hub (or, with
`DOLMEN_EMBED_MODEL` naming a directory, fails until the directory is fixed). The round-trip above
remains the end-to-end verification: with `huggingface.co` unreachable and the cache correctly
pre-seeded, the insert and the `search_vector(text=...)` load the model from disk; a missing or
incomplete cache fails at that first operation with the offline remediations in the error message.

Local provider notes:

- **Model cache** lives at `<data>/models/` (one `org--name` directory per model). Set
  `REMBED_CACHE` to override the location, and `HF_TOKEN` for gated Hugging Face repos.
- **e5 role prefixes are automatic.** The e5 family's `"query: "` / `"passage: "` prefixes are
  prepended server-side (see "Choosing an embedding model"); symmetric models, including the
  default MiniLM, are embedded as-is. Models with other instruction contracts (bge, arctic) still
  rank worse than they should — prefer the e5 family or a symmetric model.
- **Offline installs**: pre-seed the model cache under `<data>/models/` (the tarball extracts to
  the right `org--name` layout), or set `DOLMEN_EMBED_MODEL` to an absolute model-directory path.
  Both forms skip the Hub entirely when `huggingface.co` is unreachable. See the
  Offline install section below.
- **Identity pinning** works as with the OpenAI provider: tables record `local/<model>` (the
  versioned `local/v2:<model>#e5` form when the prefix contract is active) as their embedding
  space, and a model change is rejected until the table is re-embedded
  (`migrate` with `set_vectorize` off, then on).

## Configuration

Dolmen reads its startup configuration from command-line flags and environment
variables. Unknown flags and positional arguments are rejected with an error.
`dolmen mcp` takes the same flags and environment and serves the MCP surface
over stdio instead of HTTP (see [MCP (agents)](#mcp-agents)).

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `-addr` | `DOLMEN_ADDR` | `127.0.0.1:8790` | HTTP listen address (`dolmen mcp` does not listen) |
| `-data` | `DOLMEN_DATA` | `data` | Data directory (one SQLite file per namespace) |
| `-engine` | `DOLMEN_ENGINE` | `sqlite` | Storage engine: `sqlite` (default) or `postgres`. `postgres` needs `-pg-dsn`; unknown values are rejected with an error |
| `-pg-dsn` | `DOLMEN_PG_DSN` | — | PostgreSQL connection string; required with `-engine postgres` and rejected without it |
| `-pg-catalog` | `DOLMEN_PG_CATALOG` | `dolmen_catalog` | PostgreSQL catalog schema |
| `-pg-query-role` | `DOLMEN_PG_QUERY_ROLE` | — | Pre-provisioned restricted role that caller SQL runs as; required for the `query` op |
| `-auth` | `DOLMEN_AUTH` | `off` | Authentication. `off` is the v0.2.0 behavior: no identity, no credential, bind to loopback. `on` is deny-by-default and refuses to start without an identity source and a reachable root administrator, which on first start means `DOLMEN_ADMIN_KEY` (see [Authentication](#authentication)) |
| — | `DOLMEN_ADMIN_KEY` | — | Bootstrap admin credential. Needed with `-auth on` until another source yields a root administrator, and removable after the hand-over (see [Permissions](#permissions)). 32–256 characters of `[A-Za-z0-9_-]`, presented as `Authorization: Bearer <key>`. Environment only — flags are visible in process listings |
| — | `DOLMEN_SECRET_KEY` | — | Key that encrypts `secret` fields at rest (AES-256-GCM) on either engine: 32 bytes, base64-encoded (`openssl rand -base64 32`). Without it, creating a secret field or writing a secret value is refused. Environment only; never logged |
| — | `DOLMEN_SECRET_KEY_FILE` | — | Path to a file holding that key instead. Set at most one of the two |
| — | `DOLMEN_SECRET_KEYS_OLD` | — | Retired secret keys, comma-separated base64, used only to decrypt values written before a key rotation (see "Rotating the secret key"). Needs `DOLMEN_SECRET_KEY`. Environment only |
| — | `DOLMEN_SECRET_KEYS_OLD_FILE` | — | Path to a file holding those retired keys, one per line, instead. Set at most one of the two |
| `-trusted-proxies` | `DOLMEN_TRUSTED_PROXIES` | — | Comma-separated CIDRs (bare IPs allowed) whose peers may assert `X-Dolmen-Principal` / `X-Dolmen-Groups`. The same peers are the only ones whose forwarding headers (`X-Forwarded-Host`/`-Proto`/`-Prefix`, `Forwarded`, original-URI headers) shape the public links dolmen advertises; from other peers they are dropped. Trust is decided from the immediate TCP peer, never from `X-Forwarded-For` |
| `-max-groups` | `DOLMEN_MAX_GROUPS` | `128` | Maximum group entries accepted per request, `1` to `1024`. An over-limit list fails the identity rather than dropping a group |
| — | `DOLMEN_AUTH_OIDC_ISSUER` | — | Identity provider issuer URL. Enables sign-in at `/v1/auth/begin` (see [Signing in through an identity provider](#signing-in-through-an-identity-provider)) |
| — | `DOLMEN_AUTH_OIDC_CLIENT_ID` | — | OAuth client id registered with the provider |
| — | `DOLMEN_AUTH_OIDC_CLIENT_SECRET` | — | OAuth client secret. Environment only |
| — | `DOLMEN_AUTH_OIDC_PRESET` | — | `github` signs in with GitHub instead of a generic OIDC provider. Refused alongside `DOLMEN_AUTH_OIDC_ISSUER` |
| — | `DOLMEN_AUTH_OIDC_SCOPES` | — | Comma-separated extra scopes to request |
| — | `DOLMEN_AUTH_OIDC_GROUPS_CLAIM` | `groups` | Claim carrying the caller's groups |
| — | `DOLMEN_AUTH_OIDC_TOKEN_TTL` | `168h` | Lifetime of an issued token, `1h` to `720h` |
| — | `DOLMEN_AUTH_OIDC_DEPLOYMENT_ID` | minted on first start | Pins this deployment's token issuer id. A mismatch against the stored value is refused at startup |
| `-version` | — | — | Print version and exit |
| `-prefix` | `DOLMEN_PREFIX` | — | Mount all endpoints (`/livez`, `/readyz`, `/healthz`, `/metrics`, `/version`, `/skills*`, `/v1/*`, `/mcp`) under this URL prefix. Use with a pass-through proxy that forwards the full path |
| `-base-url` | `DOLMEN_BASE_URL` | — | Public base URL for the links rendered into the skills manifest, the skill markdown, and the MCP `initialize` instructions. Default: derive from the request `Host` and forwarded headers. Refused when it ends with `-prefix` |
| `-vector-cache-size` | `DOLMEN_VECTOR_CACHE_SIZE` | `512MiB` | Memory for decoded vectors kept between vector searches, shared by all tables; about 4 bytes per dimension plus about 125 bytes per row. `0` disables it and every search reads vectors from disk. Results are identical either way. SQLite engine only |
| `-max-namespace-size` | `DOLMEN_MAX_NAMESPACE_SIZE` | `0` | Largest a namespace file may grow, as bytes or with `KiB`/`MiB`/`GiB`/`TiB`. A write that would pass it is refused with `507` and writes nothing; reads keep working. `0` is unbounded. SQLite engine only (see [Disk use](docs/deployment.md#disk-use)) |
| `-log-level` | `DOLMEN_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, or `error`. `debug` adds one line per operation over HTTP, MCP or stdio with the operation, outcome code, status, duration, request size, and request id, and never the payload, SQL, arguments, or credentials |
| `-sync` | `DOLMEN_SYNC` | `full` | Commit durability. `full`: an acknowledged commit survives power loss. `normal`: it survives a process crash, but the last commits before a power or OS failure may be lost, for faster writes. Each namespace's writer is checked at open, and a mismatch is refused. SQLite engine only (see [Durability](docs/deployment.md#durability)) |
| `-shutdown-grace` | `DOLMEN_SHUTDOWN_GRACE` | `60s` | On SIGTERM, how long running requests may finish before they are cancelled (an open transaction rolls back). `0` waits without a bound; otherwise `1s` to `24h`. See [Shutting down](docs/deployment.md#shutting-down) |
| `-change-retention` | `DOLMEN_CHANGE_RETENTION` | `168h` | Change-log retention for `changes_since` / `wait_for` / `subscribe`. `0` disables pruning (records and cursors never expire); otherwise `1h` to `2160h` |
| `-max-subscription-age` | `DOLMEN_MAX_SUBSCRIPTION_AGE` | `30m` | `subscribe` connection age bound: the stream teaching-closes at the bound and the client reconnects from its cursor. `0` disables the bound; otherwise `1s` to `24h` |
| `-read-timeout` | `DOLMEN_READ_TIMEOUT` | `2m` | Time to read one request, headers and body; a body that arrives too slowly is answered `408` with code `timeout`. `0` disables the bound; otherwise `1s` to `24h` |
| `-write-timeout` | `DOLMEN_WRITE_TIMEOUT` | `2m` | Time to write one response once it starts, so a client that stops reading cannot hold the connection. `subscribe` streams bound each frame instead. `0` disables the bound; otherwise `1s` to `24h` |
| `-idle-timeout` | `DOLMEN_IDLE_TIMEOUT` | `2m` | Time a keep-alive connection may wait for its next request. `0` disables the bound; otherwise `1s` to `24h` |
| `-max-header-bytes` | `DOLMEN_MAX_HEADER_BYTES` | `1048576` | Largest request header block accepted; larger ones are answered `431`. `4096` to `16777216` |
| `-op-timeout` | `DOLMEN_OP_TIMEOUT` | `2m` | Time for one operation's work over HTTP, MCP or stdio; `wait_for` gets its `timeout_ms` on top, and `migrate` answers to `-migrate-timeout` instead. An operation past it is stopped and answered `504` with code `timeout`. `0` disables the bound; otherwise `1s` to `24h` |
| `-migrate-timeout` | `DOLMEN_MIGRATE_TIMEOUT` | `0` | Time for one `migrate` call, which may backfill every row inside one transaction. `0` leaves it unbounded; otherwise `1s` to `24h` |
| `-max-open-namespaces` | `DOLMEN_MAX_OPEN_NAMESPACES` | `128` | Namespaces held open at once. Past it, the least recently used idle namespace closes and reopens on its next request; one in use is never closed. At least `1`; SQLite engine only (see [Open namespaces and file descriptors](docs/deployment.md#open-namespaces-and-file-descriptors)) |
| — | `DOLMEN_SKILL_NAMESPACE_HINT` | built-in default | Hint text rendered into the served skill markdown |
| — | `DOLMEN_ALLOWED_ORIGINS` | — | Comma-separated allowed HTTP origins for CORS; `localhost`, `127.0.0.1`, and `::1` are always allowed |
| — | `DOLMEN_EMBED_PROVIDER` | `local` | Embedding provider: `local` (built-in in-process embeddings via [rembed](https://github.com/rostamlabs/rembed), default), `openai` (any OpenAI-compatible endpoint), or `none` (caller supplies vectors). Unknown values produce an error |
| — | `DOLMEN_EMBED_BASE_URL` | `https://api.openai.com/v1` | Base URL for an OpenAI-compatible provider |
| — | `DOLMEN_EMBED_MODEL` | provider default | Model: `sentence-transformers/all-MiniLM-L6-v2` for `local` (or an absolute model-directory path; `intfloat/multilingual-e5-small` for mixed-language/CJK — e5-family ids get `query:`/`passage:` role prefixes automatically), `text-embedding-3-small` for `openai` |
| — | `DOLMEN_EMBED_API_KEY` | — | API key for an OpenAI-compatible provider. If set (even to `""`), it takes precedence over `OPENAI_API_KEY` |
| — | `OPENAI_API_KEY` | — | Fallback API key when `DOLMEN_EMBED_API_KEY` is unset |
| — | `REMBED_CACHE` | `<data>/models` | Model cache directory for the `local` provider (overrides the data-dir location) |
| — | `HF_TOKEN` | — | Hugging Face token for gated repos downloaded by the `local` provider |

## Authentication

Dolmen defaults to `-auth off`: no credential is required, no identity exists, and
the server binds to loopback. That is the right mode for a local agent and it is
not going away.

`-auth on` turns on deny-by-default. Identity can come from four sources: the
bootstrap admin key, a gateway's headers, API keys, and sign-in through an
identity provider, each covered below. The admin key comes first, because it is
how the first grants get made:

```bash
DOLMEN_AUTH=on \
DOLMEN_ADMIN_KEY="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')" \
./dolmen -addr 0.0.0.0:8790
```

Callers then present the key as a bearer token:

```bash
curl -sS http://localhost:8790/v1/list_namespaces \
  -H "Authorization: Bearer $DOLMEN_ADMIN_KEY" \
  -H 'Content-Type: application/json' -d '{}'
```

Every `/v1/{op}`, `/mcp`, and `/v1/subscribe` request without an accepted
credential answers `401` with error code `unauthorized`. Rejections are
deliberately uniform — a wrong key, a malformed one, and a missing one produce
the same message, so the response never says which part failed.
`/livez`, `/readyz`, `/healthz`, `/metrics`, `/version`, `/skills*`, and `/v1/openapi.json` stay unauthenticated
in both modes: they are probes, counters and client-side schema discovery, and
expose no row data. `/metrics` reveals request volume per operation; if that matters, block it at your proxy.

### Identity from a gateway

If dolmen sits behind an authenticating proxy, the proxy can assert the caller's
identity with headers, and dolmen will trust them only from peers inside
`-trusted-proxies`:

```bash
DOLMEN_AUTH=on \
DOLMEN_ADMIN_KEY="$key" \
DOLMEN_TRUSTED_PROXIES=10.0.0.0/8 \
./dolmen -addr 0.0.0.0:8790
```

The proxy then sends `X-Dolmen-Principal: alice` and, optionally,
`X-Dolmen-Groups: team-a,readers`. Trust comes from the TCP peer address, never
from `X-Forwarded-For`, which any client can spoof; headers from a peer outside
the configured ranges are ignored entirely. A malformed principal or group, an
over-limit group list, or an attempt to assert the reserved `dolmen-admin`
principal is a `401`, never a silently weakened identity. A bearer credential
outranks an asserted header, and an invalid bearer is refused rather than
falling back to the header identity.

### Permissions

An authenticated caller starts with nothing. Access comes from **grants**: a
subject (a principal or a group) gets **verbs** on an **object** (a namespace, a
table, or `*` for the whole server).

The seven verbs are `create`, `read`, `update`, `delete`, `schema`, `admin`,
and `reveal`. The first six are CRUD-shaped on purpose — an append-only table is
`create` without `update` or `delete`, which a bundled "write" permission could
not express. `reveal` returns `secret` fields in plaintext; `admin` does not
imply it, and every reveal is written to the audit log without the value.

```bash
# The bootstrap admin key can grant. Give a group read access to a namespace:
curl -sS http://localhost:8790/v1/grant \
  -H "Authorization: Bearer $DOLMEN_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"subject":{"type":"group","id":"team-a"},
       "object":{"namespace":"acme"},
       "verbs":["read"]}'
```

Grants inherit **downward**: a grant on `acme` covers every table in it and
every namespace under it; `*` covers everything. They never inherit upward — a
grant on one table says nothing about its namespace or its siblings. A caller's
effective verbs are the union of every grant matching their principal and every
grant matching any of their groups, over the object and everything covering it.
There are no deny grants and no precedence — only union.

Grants are idempotent by (subject, object): re-granting merges new verbs into
the existing grant and keeps its `created_at`. `revoke` takes explicit verbs,
never an implicit "all", and the grant disappears when its last verb goes.
Dropping a namespace or table removes the grants targeting it.

A few rules worth knowing:

- **Authorization precedes existence.** An ungranted caller gets `403` whether
  or not the object exists, and `list_tables` on a namespace they hold nothing
  under answers `404` — so neither can be used to enumerate names.
- **`query` needs `read` on the whole namespace**, not one table: raw SQL can
  reference any table in it.
- **`drop_table` needs `admin` as well as `schema`**, because dropping a table
  deletes the grants targeting it, and changing what others may do is `admin`.
- **Implicit namespace creation is off** when auth is on. A write to a namespace
  that does not exist answers `404` instead of creating it, which would bypass
  the `admin` grant on the parent.
- **Grant-free ops still work**: `describe_server`, `capabilities`,
  `infer_schema`, `whoami`, and `list_namespaces` (which lists only what the
  caller can reach).

After a `403`, `whoami` reports the principal, groups, and identity source the
request authenticated as — enough to ask an administrator for the right grant.

**Bootstrapping and handing over.** The admin key exists to mint the first real
administrators, not to reign. Grant `admin` on `*` to a principal, then remove
`DOLMEN_ADMIN_KEY` from the environment and restart: the bootstrap identity
disappears while the grants persist. Revoking the last root administrator is
refused while no replacement exists, and setting `DOLMEN_ADMIN_KEY` again always
recovers a locked-out deployment.

### Per-row ownership

A table can restrict rows to whoever wrote them:

```bash
curl -sS http://localhost:8790/v1/create_table \
  -H "Authorization: Bearer $DOLMEN_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"namespace":"acme","table":"notes",
       "fields":[{"name":"body","type":"text"}],
       "row_access":"own"}'
```

The server adds an implicit `owner` column and stamps it on every insert.
Callers never supply it — it is refused like any unknown field. What a caller
sees then depends on the verbs they hold on the table:

| Caller holds | Sees |
|---|---|
| `read` | every row (the `read` verb is table-wide by definition) |
| any of `create`, `update`, `delete` | only the rows they wrote |
| only `schema` or `admin` | no rows; `describe_table` reports `row_count` 0 |

> **Never grant `read` to the people a `row_access` table keeps apart.** `read`
> shows every owner's rows, even alongside other verbs. For "each person sees
> only their own rows", grant `create` (plus `update`/`delete` if they edit), and
> keep `read` for whoever should see everyone's rows, such as an auditor.

Own-row visibility rides with **any** data verb, so a `create`-only appender can
search back what it appended without being able to read anyone else's rows.
When teams must not see each other's schema at all, a sub-namespace per team is
the other tool; [Separating tenants](docs/deployment.md#separating-tenants)
compares the two.

`row_access` can be turned on later with `migrate`, but only while the table is
empty — there is no honest way to assign owners to rows that already exist,
since no operation can write another principal's rows as that principal. To
adopt it for existing data, create a new table and replay each owner's rows
under their own identity. Turning it off keeps the column and its values and
needs `admin` as well as `schema` and `read`, because it changes what every
other data-verb holder may reach.

**Filters work under a scope, over your own rows only.** `update`, `delete`,
`upsert` and filtered searches take a SQL `WHERE` fragment, and under auth that
fragment is checked against a fixed allowlist and then evaluated behind a
barrier: the scope is applied first, so a filter never runs against a row you
cannot see. That matters beyond the rows it returns — an expression that merely
*errors* on a foreign row would report what that row holds.

**Idempotency keys belong to whoever used them.** A key is unique per table
*and owner*, so two principals using the same string are using two different
keys: neither conflicts with the other, neither reveals the other, and the
first to use a key cannot squat it against everyone else. A retry consults only
your own domain, so it finds your record through grant changes and even after
`row_access` is turned off — the place it looks can never move under you.
Records written before authentication was turned on are kept, and replay to
callers holding table-wide `read`, whose ids those already are.

`upsert_by_key` matches on the natural key within your visible set, not across
the table. A key held by a row you cannot see counts as no match at all, so the
insert branch runs and you get a fresh row of your own; the two rows then
coexist under the same key, which is what natural keys do when several people
use them. Nothing in the response — ids, counts, or errors — distinguishes a key
someone else is using from one nobody is using.

### API keys

A machine cannot do an interactive sign-in, and a shared long-lived token is the
wrong shape for a CI job. Mint a key instead:

```bash
curl -sS http://localhost:8790/v1/create_key \
  -H "Authorization: Bearer $DOLMEN_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"ci runner","principal":"ci-bot","groups":["builders"]}'
```

The response carries the credential **once** — it is stored hashed and can never
be shown again. Mint a new key if it is lost. The caller presents it exactly
like any other bearer token:

```bash
curl -sS http://localhost:8790/v1/query \
  -H "Authorization: Bearer dlm_..." \
  -H 'Content-Type: application/json' -d '{"namespace":"acme","sql":"SELECT 1"}'
```

A key **grants nothing by itself**. It authenticates as its principal, and that
principal needs grants like anyone else — which is what makes it safe to mint
one before deciding what it may do. Group grants reach key identities too, so a
fleet of machines can share one grant through a group.

Every key carries a server-generated id. `revoke_key` selects by that id, so two
keys sharing a name and principal stay individually revocable — you can drop
exactly the compromised credential. `list_keys` reports ids, names, principals,
groups and revocation state, never credentials. A revoked key is refused with
the same `401` as an unknown one.

Revoking a key is refused when it would leave the deployment with no usable root
administrator; setting `DOLMEN_ADMIN_KEY` and restarting always recovers.

### Signing in through an identity provider

For humans, dolmen can run the sign-in itself — no gateway, no proxy:

```bash
DOLMEN_AUTH=on \
DOLMEN_ADMIN_KEY="$key" \
DOLMEN_AUTH_OIDC_ISSUER=https://login.microsoftonline.com/<tenant>/v2.0 \
DOLMEN_AUTH_OIDC_CLIENT_ID=... \
DOLMEN_AUTH_OIDC_CLIENT_SECRET=... \
./dolmen -addr 0.0.0.0:8790
```

Register `<base-url>/v1/auth/callback` as the redirect URI with your provider,
then send people to `/v1/auth/begin`. They sign in with the provider, land back
on a small page carrying a token, and present it as a bearer credential like any
other. `DOLMEN_AUTH_OIDC_PRESET=github` uses GitHub instead of a generic OIDC
provider; it brings its own issuer, so setting it alongside
`DOLMEN_AUTH_OIDC_ISSUER` is refused at startup rather than silently preferring
one. Those two endpoints are the only non-JSON surface dolmen serves, and they
appear in `/v1/openapi.json` only on a deployment that serves them.

**dolmen stores no users and no sessions.** The token is Ed25519-signed and
stateless, valid for `DOLMEN_AUTH_OIDC_TOKEN_TTL` (default 7 days, range 1h to
720h). When it expires the caller signs in again. There is no per-device
revocation and no sliding session. To sign everyone out at once, rotate the
signing key and retire its predecessor:

```bash
curl -sS http://localhost:8790/v1/rotate_signing_key \
  -H "Authorization: Bearer $DOLMEN_ADMIN_KEY" \
  -H 'Content-Type: application/json' -d '{"retire_previous":true}'
```

Without `retire_previous` the successor signs new tokens while the predecessor
keeps verifying, so tokens already in people's hands live out their TTL — the
overlap you want for a routine rotation. With it, every token signed by an
earlier key stops working.

On the replica that served the rotation the change is immediate. Others pick it
up within their keyring refresh interval (30 seconds), since the keyring lives in
the shared registry rather than in any one process.

**Identities are qualified by issuer.** A subject is only unique within its
provider, so the principal is `oidc:v1:<issuer-digest>:<sub>` — never the email,
which changes. Groups are qualified the same way. Two consequences worth
knowing: changing `DOLMEN_AUTH_OIDC_ISSUER` mints a completely separate set of
principals and inherits no grants, and Entra emits group claims as object GUIDs
rather than names, so Entra deployments grant on the GUIDs or sync names
themselves.

The signing key and the deployment's own issuer id are minted on first start and
persist beside the grants, so tokens survive restarts and verify across
replicas. Pin the id with `DOLMEN_AUTH_OIDC_DEPLOYMENT_ID` if you need it
stable; a mismatch against the stored value is refused at startup, naming both.
Two deployments that accidentally share a keyring still will not accept each
other's tokens, because the id is checked as well as the signature.

`dolmen mcp` (the stdio transport) refuses to start with `-auth on`: a pipe
carries no per-request credential, and treating whoever launched the subprocess
as an administrator would be a silent bypass. Stdio is reachable only by its
parent process, so run it with auth off.

Running authentication for a team — which identity sources to enable, TLS, the
first start and hand-over, what a gateway must do, and how sign-in behaves across
replicas — is covered in the [deployment guide](docs/deployment.md).

## Reverse proxy / sub-path hosting

Dolmen can be exposed at a sub-path behind a reverse proxy in two ways. In both
recipes the public links rendered by the skills manifest, the skill markdown,
and the MCP initialize instructions must match the URL the client uses.

### Stripping proxy

The proxy removes the sub-path before forwarding to dolmen. Set the public base
URL explicitly or rely on forwarded headers (`X-Forwarded-Proto`,
`X-Forwarded-Host`, `X-Forwarded-Prefix`).

Forwarding headers count only from a peer listed in `-trusted-proxies` /
`DOLMEN_TRUSTED_PROXIES`; from any other peer they are dropped, so a client
cannot make dolmen advertise links to another host. For a proxy on the same
machine, that means `DOLMEN_TRUSTED_PROXIES=127.0.0.1` (add `::1` if it connects
over IPv6). With `-auth on`, a listed peer may also assert identity through
`X-Dolmen-Principal` / `X-Dolmen-Groups`, so a proxy you trust only for its
URL must remove those headers from what clients send
(`proxy_set_header X-Dolmen-Principal "";` and the same for `X-Dolmen-Groups`
in nginx).

nginx:

```nginx
location /dolmen/ {
    proxy_pass http://127.0.0.1:8790/;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header X-Forwarded-Prefix /dolmen;
}
```

Caddy (strips the matched prefix automatically):

```caddy
handle_path /dolmen/* {
    reverse_proxy 127.0.0.1:8790 {
        header_up X-Forwarded-Prefix /dolmen
    }
}
```

With the forwarded header and the proxy listed in `DOLMEN_TRUSTED_PROXIES`, no
other dolmen configuration is needed. If your proxy does not add `X-Forwarded-*`
headers, or you would rather not list it, set `DOLMEN_BASE_URL` to the full
public URL instead:

```bash
DOLMEN_BASE_URL=https://example.com/dolmen ./dolmen
```

### nginx `rewrite`

A `rewrite` that strips the sub-path is a stripping proxy, and it is the easiest
one to get wrong, because nginx supplies none of the context dolmen needs:
`Host` defaults to the **upstream** address (`127.0.0.1:8790`), and nginx never
sends `X-Forwarded-Prefix` on its own. Left alone, dolmen advertises links to
its own loopback address with no sub-path.

Dolmen recovers the sub-path by itself when a trusted proxy forwards the
original request URI — nginx's `$request_uri` — so this config, with
`DOLMEN_TRUSTED_PROXIES=127.0.0.1`, works without naming the prefix twice:

```nginx
location /dolmen/ {
    rewrite ^/dolmen/(.*)$ /$1 break;
    proxy_pass http://127.0.0.1:8790;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header X-Original-URI $request_uri;
}
```

`X-Forwarded-Uri`, `X-Original-URL`, `X-Envoy-Original-Path`, and
`X-Rewrite-URL` are read the same way; an explicit `X-Forwarded-Prefix` always
wins over inference. The standard `Forwarded` header (RFC 7239) is honored for
`proto` and `host`, below the `X-Forwarded-*` equivalents.

Setting `DOLMEN_BASE_URL` to the full public URL overrides all of it and is the
surest fix when a proxy cannot be changed.

### Troubleshooting sub-path links

Ask the server what it thinks its public URL is, through the proxy:

```bash
curl -s https://example.com/dolmen/skills | grep base_url
```

If `base_url` is not the URL you typed, the proxy is not telling dolmen enough.
The server also logs a warning the first time it advertises a base URL that no
proxied client could reach:

```
level=WARN msg="advertising a base URL no proxied client can reach" base_url=http://127.0.0.1:8790
```

### Pass-through proxy

The proxy forwards the full path, including the sub-path, to dolmen. Run dolmen
with `-prefix` (or `DOLMEN_PREFIX`):

```bash
./dolmen -prefix /dolmen
```

nginx:

```nginx
location /dolmen/ {
    proxy_pass http://127.0.0.1:8790;  # no trailing slash: do not strip
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

Caddy:

```caddy
handle /dolmen/* {
    reverse_proxy 127.0.0.1:8790
}
```

`-base-url` and `-prefix` can be combined when the configured base URL does not
already end with the prefix. For example, `-base-url https://example.com
-prefix /dolmen` produces public links at `https://example.com/dolmen`. To avoid
double-prefixing, dolmen refuses to start when `-base-url` ends with `-prefix`.

### Browser MCP clients

When a browser-based MCP client connects via a proxy, the browser sends an
`Origin` header such as `https://example.com`. Add it to the allowlist:

```bash
DOLMEN_ALLOWED_ORIGINS=https://example.com ./dolmen
```

`localhost`, `127.0.0.1`, and `::1` are always allowed; the public origin of a
proxy is not.

## Embedded library use (Go)

Go programs can open dolmen in-process — no port, IPC, or subprocess. The module is `github.com/lsm/dolmen`; the executable's install path is now `github.com/lsm/dolmen/cmd/dolmen` (historical release instructions describe their own layout):

```go
import "github.com/lsm/dolmen"

st, err := dolmen.Open(dataDir,
	dolmen.WithEmbedding(myProvider),
	dolmen.WithChangeRetention(168*time.Hour),
)
if err != nil { ... }
defer st.Close()
```

`Open` never reads environment variables, loads a model, or starts a server; options are validated before touching the filesystem. Embedding is disabled unless a provider is supplied — implement `dolmen.EmbeddingProvider` (`Identity`, `Embed`, `EmbedQuery`); query and document embedding stay distinct for asymmetric models such as E5, and the identity string pins a vectorized table to its embedding space. The application owns the provider (a shared provider is not closed by `Close`).

The surface covers namespace/table lifecycle, `Insert`/`Update`/`Delete`/`UpsertByKey`, `GetRows`/`Query`, and `SearchFulltext`/`SearchVector` — the same semantic validation, embedding orchestration, error classification, and defaults as the HTTP and MCP transports (the conformance suite pins the parity). Records are `map[string]any`; reads return engine-typed values (`int64`/`float64` numbers, `bool`, decoded JSON with `json.Number` precision, `[]float64` vectors) rather than a JSON round trip.

Errors are typed: match categories with `errors.Is(err, dolmen.ErrConflict)` (also `ErrNotFound`, `ErrQuery`, `ErrInvalidRequest`, `ErrEmbedderUnavailable`, `ErrCanceled`, `ErrForbidden`, `ErrInternal`) and read `*dolmen.Error` with `errors.As` for the code and message; underlying causes stay wrapped. `Close` is terminal and idempotent — repeated Close returns the first result; operations after close return the local `dolmen.ErrClosed` sentinel — and one process holds at most one live store per data directory (symlinks resolve; opening a server and an embedded store on the same directory is unsupported).

While dolmen is on v0, breaking Go API changes land in minor releases only; patch releases keep source and behavioral compatibility.

A runnable end-to-end example lives in [`examples/basic`](examples/basic/main.go).

## MCP (agents)

Dolmen speaks MCP over two transports — one dispatcher, two framings: `tools/list`, `tools/call`, the JSON-RPC 2.0 envelopes, and the error taxonomy are identical either way.

**HTTP** (`./dolmen`) — one server, many clients, browser and curl access:

```bash
claude mcp add --transport http dolmen http://127.0.0.1:8790/mcp
```

**stdio** (`dolmen mcp`) — for hosts that launch the server as a subprocess (Claude Desktop, Cursor, any non-Go client that can spawn a process). One process serves one data directory: newline-delimited JSON-RPC 2.0 on stdin/stdout, stdout carries protocol only (all logs go to stderr), and the process serves until stdin closes or SIGTERM/SIGINT arrives, then drains in-flight requests (a 5s grace, then cancellation); a second SIGINT/SIGTERM during a stuck drain kills the process immediately:

```bash
claude mcp add dolmen -- /path/to/dolmen mcp -data /path/to/data
```

`dolmen mcp` takes the same flags and environment as `dolmen`; the HTTP-only ones (`-addr`, `-max-subscription-age`, `DOLMEN_ALLOWED_ORIGINS`) have no effect. The `initialize` handshake works as over HTTP (its instructions describe the stdio transport; set `-base-url` when an HTTP deployment also exists, and its links appear there). Over stdio there is no `MCP-Protocol-Version` header, so version negotiation happens in `initialize` alone.

The MCP server exposes the same twenty-six operations as tools (`tools/list` shows them with input/output schemas and annotations). Successful `tools/call` results carry `structuredContent` — the result as a JSON object matching the tool's `outputSchema` — with no text mirror (`content` stays an empty array: the spec keeps it mandatory); tool errors are reported as text with `isError: true`.

Skill distribution is built into the server. `GET /skills` returns a JSON manifest with links to the layered skill markdown; `GET /skills/dolmen` is the end-user skill and `GET /skills/dolmen-admin` is the developer skill. Agents should fetch the skill from the running binary instead of copying a static file. Over `dolmen mcp` there is no HTTP listener — the skills are served by the HTTP deployment named by `-base-url`, when one exists.

## Live changes (SSE subscribe)

`GET /v1/subscribe` streams a namespace's change feed over server-sent events: it replays the
durable change log from a cursor, then delivers every live commit as it happens. It is the push
counterpart of `wait_for`, which carries the same feed semantics as a request/response tool;
`capabilities`'s `subscribe` field reports whether the server offers it.

```bash
curl -sN "http://127.0.0.1:8790/v1/subscribe?namespace=myapp"
```

Query parameters mirror `changes_since` (names are trimmed and lowercased like every `/v1`
call): `namespace` (required — a namespace that does not exist is an in-stream `not_found`
error; the stream never creates one, as no read does), `table` (optional
filter to one table's feed; an explicitly empty value is rejected), and `cursor` (an opaque
resume token or the literal `begin`; omitted = start at the current head and receive future
commits only; an explicitly empty value is rejected). Wrong method, an omitted `namespace`
parameter, and empty `table` / `cursor` values are ordinary HTTP errors (the standard envelope)
before the stream opens; every
other failure arrives inside the stream, because an open `text/event-stream` response can no
longer carry an HTTP status.

The frame protocol — named events whose `data` is one compact JSON line:

- `event: ready`, `data: {"cursor":"..."}` — the replay→live boundary. Replayed `change` frames
  (when resuming behind the head) arrive first, then `ready`, then live frames. Its cursor is the
  last replayed change's, or the head at registration when nothing replayed — reconnect with it
  to resume exactly there, gap-free. A stream that ends during replay never reaches `ready`; its
  `close` frame carries the reached cursor instead.
- `event: change`, `data: {"cursor":"...","table":"...","row_id":1,"kind":"insert"}` — one frame
  per changed row: a batch write (multi-record `insert`, filter-matched `update`/`delete`) mints
  one change-log record per affected row, and frames arrive in commit order. `kind` is `insert`,
  `update`, or `delete`. The payload is the same identity projection `changes_since` returns —
  re-read the row by id; a `delete` names a row that is already gone.
- `event: close`, `data: {"cursor":"..."}` — sent before every server-initiated terminal, with
  the last-delivered cursor: the reconnect token.
- `event: error`, `data: {"ok":false,"error":{"code","message","request_id"}}` — the terminal
  frame: the standard error envelope. Nothing follows it.

Between change deliveries an idle stream sends a `: keepalive` comment frame every 20 seconds —
an SSE comment line with no event name and no data, ignored by every event parser, carrying no
cursor and never advancing one. Treat it as liveness: a connection that delivers neither a
change nor a keepalive for a couple of intervals (~40 s) is dead — close it and reconnect from
your last cursor.

Every server-initiated terminal is a `close` frame followed by a teaching `error` event whose
message is the recipe; registration failures — an unknown or foreign cursor, a missing namespace,
a missing table — send only the `error` event, since nothing was delivered. The causes, verbatim:

- Unknown or expired cursor: "cursor is unknown or past the change-log retention window
  (-change-retention, default 168h); catch up by reconnecting with no cursor to resume from the
  current head, or with cursor=begin to replay retained history"
- Cursor from another feed: "cursor was minted on a different feed (a specific table's, or the
  namespace-wide feed); pass it only to the feed you received it from — honoring it elsewhere
  would silently skip events — or start fresh with no cursor / cursor=begin"
- Buffer overflow: "subscription buffer overflow: commits arrived faster than this stream drained
  them; reconnect from the cursor in the preceding close frame — the durable log is the catch-up
  path, the buffer never was"
- Target ended: "the subscription's target ended (a dropped table, or a dropped or replaced
  namespace); reconnect against the current target — a same-named successor is a different feed"
- Authorization revoked (code `forbidden`): "subscription authorization was revoked; reconnect once authorization
  is restored". The server rechecks access on every change it delivers and on every keepalive (every 20 seconds),
  so a revoked caller's stream ends within about 20 seconds even when nothing is being written.
- Subscription age bound (`-max-subscription-age`, default 30m): "subscription reached the
  maximum subscription age (-max-subscription-age, default 30m); reconnect from the cursor in
  the preceding close frame to resume exactly where this stream ended — the fresh connection
  re-asserts your credentials"

Recovery is one rule for the cursor-reusable terminals — buffer overflow, revoked, age bound:
reconnect with the `close` frame's cursor (or the newest you hold, from `ready` or the last
`change`, when the connection died without one) and the new stream replays everything after it,
then goes live — no gaps, no duplicates. Target ended is the close-bearing exception: the feed
you were reading is gone (a dropped table's feed, or a dropped/replaced namespace whose cursor
store was deleted with it), so its cursor cannot resume anything — reconnect against the current
target and establish a fresh cursor (no cursor, or `cursor=begin`), as its message says; a
same-named successor is a different feed. The registration failures above are the other
exception: their cursor was rejected, so reconnect exactly as their message says — no cursor
(head) or `cursor=begin` — never the rejected token. Like every feed surface, cursor tokens are
minted per emission: the same commit carries a different token on each read that re-emits it,
while a read that emits nothing returns your cursor unchanged (the `wait_for` idle contract) —
and no frame field is a stable event id either: the same row updated twice yields two changes
with identical `table`/`row_id`/`kind`. Make processing idempotent and persist the cursor
atomically with your side effects rather than deduplicating on frame content.

## Tools

| Tool | Purpose |
|---|---|
| `list_namespaces` | Namespaces on this server; an optional `prefix` (a namespace path) lists only that path's subtree, recursively |
| `create_namespace` | Reserve a namespace up front (the write ops create implicitly on first use otherwise; every read — including `wait_for` and the `subscribe` stream — never creates, and a missing namespace is `not_found`) |
| `drop_namespace` | Delete a namespace and all its tables; `confirm` must repeat the name; a namespace with child namespaces is refused — drop the children first |
| `rotate_secret_key` | Re-encrypt stored `secret` values under the active key in bounded batches; reports `rotated`, `remaining`, `done` and values per key id (`admin` on `*`) |
| `vacuum` | Reclaim free space in one namespace (SQLite: rebuild the file and truncate its WAL; PostgreSQL: `VACUUM` its tables) and report `bytes_before` / `bytes_after` |
| `list_tables` | Tables in a namespace |
| `describe_server` | Server's embedding provider status — provider (`none` / `local` / `openai`), model, the identity that pins vectorized tables, whether server-side embedding is usable, and (local only) whether the model is cached; read-only, no secrets |
| `describe_table` | Schema, version, row count |
| `read_rows` | Fetch rows by id — each found row once, ascending id order; missing ids are simply absent (never an error); `truncated` is true only when the response budget dropped rows for existing ids (retry with fewer); at most 1,000 ids per request |
| `capabilities` | The engine's static capability surface — `vector_execution` (`exact` / `ann`), `ann_recall_bound` (explicit `null` when exact), `notifications`, `subscribe`, `query_dialect`, `filter_dialect`; reported verbatim |
| `create_table` | Typed fields with `fulltext` / `vector` / `vectorize` / `enum` / `shape` / `default` annotations (`enum` restricts a string field to a closed vocabulary; `shape` restricts a json field to one JSON shape such as `array<string>`; `default` is stored by inserts that omit the field — a timestamp field may declare `"now()"`, stamped server-side at write time so idempotent retries can omit the field and replay) |
| `infer_schema` | Propose fields from sample records (creates nothing). Names are always valid for `create_table`; `warnings` explains every rename or merge, `provenance` maps fields to source keys, and `evidence` counts presence, nulls and observed types per field |
| `insert` | Validated records; indexes and embeddings update automatically; `idempotency_key` makes retries replay the original ids |
| `upsert_by_key` | Insert-or-update keyed by natural field(s) (`on`); converges instead of duplicating on retry |
| `query` | Read-only SQL (SELECT/WITH), parameter binding via `args`, typed results |
| `search_fulltext` | FTS5 MATCH over `fulltext` fields, relevance-ordered, typed results; optional `filter` + `args` restrict rows before ranking |
| `tokenize` | The terms a table's full-text index stores for a piece of text, in order: use it to find the stem a prefix search must start from. Returns no row data; any data grant on the table allows it |
| `search_vector` | Cosine KNN; `text` (server embeds; searches only the vectorize `_embedding` space) or raw `vector` (any vector column, caller owns the space); optional `filter` + `args` and `min_score` threshold; results carry `_score` and `skipped_vectors` |
| `changes_since` | Replay the namespace's durable change log: changes committed after a cursor, in commit order, as a bounded page plus `next_cursor`. No cursor = start at the current head (future commits only); `"begin"` = retained history; optional `table` filters to that table's current lifetime. Changes carry `cursor`/`table`/`row_id`/`kind` only |
| `wait_for` | Long-poll the change feed: block until a change commits after the cursor or `timeout_ms` elapses (default 30000, max 60000, `0` = immediate conditional poll), then return exactly a `changes_since` page. A timeout is an empty page carrying the unchanged `next_cursor` — never an error; pass it back in to keep waiting. |
| `delete` | WHERE-filtered delete, cascades to search indexes |
| `drop_table` | Drop a table — rows, search index, schema, history, idempotency keys; `confirm` must repeat the name |
| `update` | WHERE-filtered field update; reindexes full-text rows and re-embeds changed vectorized fields |
| `upsert` | Update matching rows, or insert one record when the filter matches nothing |
| `migrate` | `add_field` (optional `default` backfills existing rows — required fields land on populated tables as `NOT NULL DEFAULT`; optional fields get a one-time backfill, later omitted inserts store NULL), `rename_field`, `drop_field`, `set_fulltext`, `set_vectorize`, `set_enum` (replaces a string field's vocabulary; rejects when a stored value falls outside the new list, naming it and its row count; an empty list removes the constraint), `set_shape` (sets or clears a json field's required shape; rejects when stored values do not fit, naming how many rows and their ids); `expected_version` asserts the schema being migrated (required for rename/drop, conflicts surface as 409), `expected_incarnation` — the opaque token a dry run returns — asserts the table itself and is what a precondition must use when auth is on, `dry_run` previews the plan without side effects; versioned + logged |
| `list_migrations` | A table's migration history, newest first, with the exact recorded changes |

## Model

- **Namespace = one SQLite file** (`data/<ns>.db`, WAL). Isolation is physical. Lifecycle is managed
  over the API: `list_namespaces`, `create_namespace`, and `drop_namespace` (which closes the server's
  own connections, then deletes the file and its WAL sidecars — `confirm` must repeat the namespace
  name, and any later **write**-op use of the name recreates the namespace empty (every read —
  including `wait_for` and the `subscribe` stream (`/v1/subscribe`) — answers `not_found` until it
  is recreated);
  a namespace with child
  namespaces is refused, the error naming the descendant count — children are dropped first, never
  deleted implicitly). Safety caveat: drop coordinates
  only within one server — another process holding the file open (a second dolmen instance, a backup
  tool) is not detected, and racing in-flight requests on the namespace may fail, so quiesce writers
  before dropping. A small registry inside each file holds table schemas, versions, and a migration
  log (surfaced by `list_migrations`).
- **Full-text** via SQLite FTS5 shadow tables, maintained on insert/update/delete/migrate.
- **Enum-validated writes**: a string field declared `enum: [...]` accepts only those values on every
  write path (`insert`, `update`/`upsert` `set`, `upsert_by_key`) — exact match, no case folding, values
  stored as written. A rejected write names the field, the rejected value, and the allowed list, over
  `/v1` and MCP alike. A declared `default` must be a member; evolve the vocabulary with `migrate`
  `set_enum`, which refuses to drop a value rows still store (naming it and its count). The annotation
  is declared and reported in the MCP `tools/list` schemas and `/v1/openapi.json`, so schema-validating
  clients see the vocabulary before the first write.
- **Shape-validated JSON**: a `json` field declared `shape` (`object`, `array`, `array<string>`,
  `array<number>`, `array<boolean>` or `array<object>`) accepts only values of that shape on every
  write path, on both engines and in the Go library alike, so a tags field declared `array<string>`
  refuses `"db,sqlite"` where `["db","sqlite"]` was meant. The refusal names the field, the expected
  shape and what arrived, down to the first misfit element's index. `null` stays allowed unless the
  field is `required`. Evolve it with `migrate` `set_shape`, which refuses a shape stored rows do not
  fit, naming how many and their ids.
- **Idempotent writes** for agent retries: `insert` accepts an `idempotency_key` (client-chosen,
  durably recorded with its ids in a side table, so a retry — even after a restart — returns the
  original ids; reusing a key for different records is an error), and `upsert_by_key` writes
  records keyed by a natural field set (`on`), updating the matched row partially or inserting
  when nothing matches.
- **Vectors** are stored as float32 blobs, and KNN is an exact cosine search in Go with no index to
  build or tune. Each table's decoded vectors are kept in memory between searches (up to
  `-vector-cache-size`, default 512 MiB) and kept current from the change log, so a search scores
  from memory across all cores instead of reading every vector back. On a recent laptop, a top-10
  search over 20,000 rows takes about 1 ms at 384 dimensions (the default `local` model) and 3 ms at
  1,536 (OpenAI's `text-embedding-3-small`); 100,000 rows take about 4 ms at 384 dimensions. A
  table's cache costs about 4 bytes per dimension plus about 125 bytes per row, so 100,000 rows at 1,536 dimensions need
  about 620 MB: raise `-vector-cache-size` to cover your largest vector tables. Searches on
  `row_access: own` tables use the cache too, since it also holds each row's owner. A `filter` is
  evaluated by SQLite first, which reads the table (about 70 ms for 20,000 rows at 384 dimensions),
  and only the matching rows are then scored from memory. A table that does not fit reads the vectors
  from disk instead, at about 200 million row-dimensions a second (0.6 s for 100,000 rows at 1,536).
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
  `vectorize`) is stripped from `SELECT *` and search results — pass `include_hidden: true` to a search
  to include it. Naming it in the SQL (outside string literals and comments) also works where the
  backend exposes it to caller SQL, but that is backend-dependent.
- **Pagination** on `query`, `search_fulltext`, and `search_vector` via `offset` and `limit` parameters.
  Do not put `LIMIT`/`OFFSET` in raw SQL; use the parameters. `search_fulltext` and `search_vector`
  have stable, deterministic ordering. The response includes `truncated: true` when more results are
  available beyond the returned page — on `query` this is triggered by the 1,000-row result cap; on
  the searches, by results existing beyond the requested `limit` (default 10, max 200).
- **Embeddings** are pluggable: `none` (caller supplies vectors), `local` (built-in in-process
  inference via [rembed](https://github.com/rostamlabs/rembed) — pure Go, no cgo, model weights
  cached under the data dir), or any OpenAI-compatible endpoint.
- Namespaces are created implicitly on first use by the **write** ops — `create_table`, `insert`,
  `update`, `upsert`, `upsert_by_key`, `delete`, and `migrate` (one file per name; `create_namespace`
  just reserves the name up front). **Reads never create.** `list_tables`, `describe_table`,
  `read_rows`, `query`, `search_fulltext`, `search_vector`, `changes_since`, `wait_for`,
  `list_migrations`, the `subscribe` stream, and `drop_table` all answer `not_found` for a namespace
  that does not exist, leaving nothing on disk — so a typo costs an error, not a stray database file.
  Tables are never implicit — call `create_table` before inserting, `drop_table` (confirm-guarded)
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

Fields marked `fulltext: true` are indexed with a shadow SQLite FTS5 table. The tokenizer is the
`porter` stemmer wrapping `unicode61`: case-insensitive, diacritic-insensitive for most Latin
characters (some non-Latin or multi-diacritic characters may not normalize), and **stemmed** — both
the index and the query reduce English words to stems, so `payments` matches `payment` and `refunds`
matches `refund` without a prefix wildcard. Most punctuation, including hyphens, is a token boundary.

Stemming notes:

- Porter is a suffix-stripper, not a lemmatizer: it collapses inflections of the same root
  (`payments`/`payment`, `refunded`/`refunds`/`refund`) but not different derivations — `paying`,
  `pays`, and `paid` stem to `pai`/`paid` and do **not** match `payment`.
- Phrases match on stems: each word of the phrase is stemmed before matching, so `"payments were"`
  matches `"the payments were refunded"` (stems `payment`, `were`).
- `tokenize` shows the terms a table's index stores for any text (`overheating` → `overh`), so a prefix search can start from the right stem.
- Prefix queries operate on stems: `pay*` stems to `pai*`, so it matches `paid`/`paying`/`pays` but
  not `payment` (whose stem is `payment`).
- Stemming is English-focused. CJK text is untouched by the stemmer — an uninterrupted CJK run is
  still indexed as one opaque token, as before (#106).
- Tables created before stemming became the default keep their exact-token index and keep working.
  Reindex one with `migrate`: `{"op": "set_fulltext", "name": "<fulltext field>", "value": true}` —
  re-asserting `true` on an already-indexed field rebuilds the index under the current tokenizer
  (the migrate plan reports `rebuild_fulltext: true`). BM25 rank ordering shifts after a reindex.

`search_fulltext` takes a raw FTS5 `MATCH` expression in `query`. It is **not** SQL, so do not wrap
the whole expression in single quotes.

Common syntax:

- `payment` — a single token (`payments` matches the same stem).
- `payment gateway` — implicit `AND` between tokens.
- `payment OR gateway` — either token.
- `payment NOT gateway` — must contain `payment` and must not contain `gateway`.
- `title:payment` — only in the `title` fulltext field.
- `{title body}:payment` — in any of the named fulltext fields.
- `"foo bar"` — phrase (adjacent tokens, matched on stems). Because stored punctuation is also
  tokenized, a phrase matches token adjacency, not literal punctuation.
- `"foo-bar"` — double-quote any term that contains spaces or punctuation (hyphens, dots, slashes,
  apostrophes). Bare `foo-bar` is read by FTS5 as a column filter and errors.
- `pay*` — prefix match, applied to the stemmed term (`pay*` → `pai*`).
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
- `_embedding` is hidden from `SELECT *` and search results unless you pass `include_hidden: true`, or
  the SQL names it explicitly on a backend that exposes it to caller SQL.

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
| Namespace path | 1–3 segments (`a/b/c`), each `^[a-z0-9][a-z0-9_-]{0,63}$` (max 64 chars per segment) | rejected |
| Table / field name | `^[a-z][a-z0-9_]{0,63}$` (max 64 chars); reserved names (`id`, `created_at`, `_embedding`, `_score`, `_rank`, `rowid`) are rejected, SQL keywords (`key`, `order`, `group`, `from`, `values` and the rest of SQLite's list) are rejected with a suggested replacement, and a field named `rank` is rejected when `fulltext: true` (reserved by the FTS5 index); table also cannot contain `__fts` or start with `sqlite_` | rejected |
| Table fields | 100 user-defined fields (not counting the implicit `id`, `created_at`, `_embedding` columns) | rejected |
| Records per `insert` / `upsert_by_key` | 1,000 | rejected |
| Ids per `read_rows` | 1,000 | rejected |
| Natural key fields per `upsert_by_key` | 8 | rejected |
| Idempotency key length | 1–256 bytes; use printable ASCII (`[ -~]`); omit the field for a non-idempotent insert | empty and over-256-byte keys are rejected; the JSON Schema enforces non-empty printable ASCII for schema-validating clients |
| Vector dimension (declared `vector` fields) | 1–4096 | rejected |
| `search_vector` query vector | at most 4096 numbers | rejected before the request is decoded |
| Search `limit` (`search_fulltext`, `search_vector`) | default 10, hard max 200 | omit `limit` for the default of 10; the tool schema enforces 1–200 for schema-validating clients, and the server clamps values above 200 to 200 (0 or negative selects the default on direct `/v1` calls); every search response reports the `limit` it applied |
| `query` result rows | 1,000 | truncated; `truncated` is `true` in the response |
| `query` / search result bytes | 32 MiB | first row over budget errors; later rows truncate; a single BLOB value over 32 MiB always errors |
| A single value built by SQL (in `query` or a search `filter`), SQLite engine | 64 MiB | `query_error` before the value is built |
| Request body size | 32 MiB | rejected with `413 Request Entity Too Large` |
| `query` `args` | 100 | rejected |
| `infer_schema` samples | 1–50 | rejected |
| Column label in `query` | 4096 bytes | rejected |

Coercion and validation rules:

- `number`: JSON numbers and Go numeric types become `int64` when integral and within the int64
  range, otherwise `float64`. A JSON number outside the int64 range is stored as `float64`, which
  loses precision — it is NOT rejected (`9223372036854775808` reads back as `9223372036854776000`).
  The separate rejection of unsigned values larger than `math.MaxInt64` applies only to the Go
  library, where the value arrives already typed.
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
- On direct `/v1` requests, namespace paths and table names are trimmed and lowercased before
  validation — a namespace per segment, so `namespace: " Production / EU "` silently operates on
  `production/eu`. The MCP tool schemas require already-canonical names, so schema-validating
  clients must send trimmed lowercase names.
- `insert` with an `idempotency_key`: the same key and the same records replay the original ids; the
  same key with different records is rejected. Use printable ASCII keys (`[ -~]`) up to 256 bytes.
- `search_vector` with `text` requires a provider and searches only the server-managed `_embedding`
  column produced by a `vectorize: true` field — the provider identity must match the one that
  embedded the table, and a `text` query naming a declared `vector` column is rejected. Searches
  with a caller-supplied `vector` need no provider and are not checked against any embedding
  space — only you know which model produced the stored and query vectors.

## Observability

Dolmen emits OpenTelemetry traces and metrics over OTLP (`http/protobuf`). Both are **off by
default**: nothing is recorded or exported until an OTLP endpoint is configured, and while they are
off the instrumentation is a no-op. They are configured only through the standard `OTEL_*`
variables (both `dolmen` and `dolmen mcp`), and each signal can be turned on or off on its own:

| Variable | Default | Effect |
| --- | --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | Setting it turns traces and metrics on, e.g. `http://collector:4318`. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | unset | A per-signal endpoint; setting one turns that signal on. |
| `OTEL_TRACES_EXPORTER` / `OTEL_METRICS_EXPORTER` | `otlp` when an endpoint is set | `otlp` turns the signal on (endpoint defaults to `http://localhost:4318`); `none` keeps it off. `prometheus` is refused for metrics: scrape `GET /metrics` instead. |
| `OTEL_SDK_DISABLED` | `false` | `true` turns traces and metrics off whatever else is set. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` / `_TRACES_PROTOCOL` / `_METRICS_PROTOCOL` | `http/protobuf` | Only `http/protobuf` is supported; gRPC is left out to keep the binary small. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `_TIMEOUT`, `_COMPRESSION`, `_CERTIFICATE`, and the `_TRACES_` / `_METRICS_` variants | | Passed to the OTLP exporters. |
| `OTEL_METRIC_EXPORT_INTERVAL` / `OTEL_METRIC_EXPORT_TIMEOUT` | `60000` / `30000` ms | How often metrics are pushed, read by the OpenTelemetry Go SDK itself. |
| `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` | `cumulative` | `delta` or `lowmemory` for backends that want deltas; read by the SDK's exporter. |
| `OTEL_LOGS_EXPORTER` | unset (off) | `otlp` also sends every log line to the collector, at the same `-log-level`; stderr always gets them. Unlike traces and metrics, an endpoint alone does not turn log export on, since logs may carry namespace names and error details an operator may not want in a telemetry backend. |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` / `_LOGS_PROTOCOL` / `_LOGS_HEADERS` ... | | The logs variants of the exporter settings. |
| `OTEL_BLRP_*` | SDK defaults | Batch log processor tuning (`OTEL_BLRP_SCHEDULE_DELAY`, `OTEL_BLRP_MAX_QUEUE_SIZE`, `OTEL_BLRP_MAX_EXPORT_BATCH_SIZE`, `OTEL_BLRP_EXPORT_TIMEOUT`), read by the SDK itself. |
| `OTEL_SERVICE_NAME` | `dolmen` | `service.name`. |
| `OTEL_RESOURCE_ATTRIBUTES` | | Extra resource attributes (`k=v,k2=v2`, percent-encoded values). |
| `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG` | `parentbased_always_on` | `always_on`, `always_off`, `traceidratio`, and the `parentbased_*` forms. |
| `OTEL_PROPAGATORS` | `tracecontext,baggage` | `tracecontext`, `baggage` or `none`. |
| `OTEL_BSP_*` | SDK defaults | Batch span processor tuning (`OTEL_BSP_MAX_QUEUE_SIZE`, `OTEL_BSP_MAX_EXPORT_BATCH_SIZE`, `OTEL_BSP_SCHEDULE_DELAY`, `OTEL_BSP_EXPORT_TIMEOUT`), read by the OpenTelemetry Go SDK itself. |
| `DOLMEN_OTEL_INCLUDE_PRINCIPAL` | `false` | `true` adds the caller's principal to operation spans as `enduser.id`. |

An unsupported value (a gRPC protocol, an unknown sampler or propagator) stops startup with an error
naming the variable. The resource carries `service.name`, `service.version`, `service.instance.id`
(random per process), host and runtime attributes. Spans are batched and flushed on shutdown, bounded
by `-shutdown-grace` (at most 10s).

What is traced:

- A SERVER span per request on `/v1/{op}`, `/mcp` and `/v1/subscribe`, following the HTTP semantic
  conventions (`http.request.method`, `http.route`, `http.response.status_code`, `url.scheme`,
  `server.address`, ...), continuing an inbound W3C `traceparent`. The subscribe span covers setup
  only and ends when the stream opens.
- A `dolmen.op <name>` span per operation, over HTTP, MCP over HTTP and MCP over stdio, with
  `dolmen.op.name`, `dolmen.op.outcome` (`ok` or the error code), `db.namespace`, `dolmen.table` and
  `dolmen.request_id` (the `X-Request-Id`). A failed operation sets the span's error status.
- An `embeddings <model>` span per embedding call (`gen_ai.operation.name=embeddings`,
  `gen_ai.request.model`, `gen_ai.provider.name`, `gen_ai.usage.input_tokens` when the provider
  reports it); CLIENT for `openai`, which also sends `traceparent` upstream, INTERNAL for `local`.
- Storage spans under the operation span, SQLite engine, for writes (`insert`, `update`, `upsert`,
  `upsert_by_key`, `delete`), both searches, `migrate` and `vacuum`: a `<db.operation.name> <table>`
  span (`INSERT docs`, `SELECT notes`, ...) carrying
  `db.system.name=sqlite`, `db.namespace` and `db.collection.name`. A write that waits for the
  namespace's single writer records that wait, so contention is visible; a vector search records
  the cache build or catch-up and the scoring pass separately, with the rows scored, the candidate
  count when a `filter` narrowed them, and whether the table was served from cache; `migrate`
  records a span per step, and `vacuum` its own. Plain reads (`read_rows`, `query`) and schema
  lifecycle calls have no storage span yet.
- The PostgreSQL engine records the same span for the same operations, as a CLIENT span (the
  database is a remote server) carrying `db.system.name=postgresql`, `db.namespace`,
  `db.collection.name`, `server.address` and `server.port`; never the SQL, its arguments or the DSN.
- Every log line written during a traced request carries `trace_id` and `span_id`. With
  `OTEL_LOGS_EXPORTER=otlp`, exported log records carry the same trace context natively, so a
  backend links them to the request's trace.

What is measured (metrics are pushed every `OTEL_METRIC_EXPORT_INTERVAL`; with traces on too, the SDK
attaches exemplars linking a slow request's histogram bucket to its trace):

| Metric | Type, unit | Attributes |
| --- | --- | --- |
| `http.server.request.duration` | histogram, `s` | `http.request.method`, `http.route`, `http.response.status_code`, `url.scheme`, `network.protocol.version`, `error.type` on 5xx |
| `dolmen.operation.duration` | histogram, `s` | `dolmen.op.name`, `dolmen.op.outcome` (`ok` or the error code); its count is the operation count |
| `dolmen.operations.in_flight` | up-down counter, `{operation}` | `dolmen.op.name` |
| `dolmen.subscriptions.active` | up-down counter, `{subscription}` | |
| `dolmen.namespaces.open` | observable up-down counter, `{namespace}` | none (SQLite) |
| `dolmen.vector_cache.usage` / `dolmen.vector_cache.limit` | observable up-down counter, `By` | none (SQLite) |
| `db.client.connection.count` | observable up-down counter, `{connection}` | `db.client.connection.state` (`idle`, `used`), `db.client.connection.pool.name` (PostgreSQL) |
| `db.client.connection.max` | observable up-down counter, `{connection}` | `db.client.connection.pool.name` (PostgreSQL) |
| `gen_ai.client.operation.duration` | histogram, `s` | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `server.address`, `error.type` on failure |
| `gen_ai.client.token.usage` | histogram, `{token}` | the same, plus `gen_ai.token.type=input`, when the provider reports usage |

Metric attributes stay low-cardinality: never namespaces, tables, principals, request ids, paths or
client addresses, so the series count is bounded by the operation and error-code lists. The dolmen
names translate to the same Prometheus names `GET /metrics` serves (`dolmen_operation_duration_seconds`,
`dolmen_operations_in_flight`, `dolmen_subscriptions_active`), so dashboards carry over between a
scrape and an OTLP pipeline.

Privacy: spans never carry SQL text, filter arguments, row payloads, embedded text, API keys or other
credentials; error statuses carry the error code, not the message. The principal (`enduser.id`) is recorded only
with `DOLMEN_OTEL_INCLUDE_PRINCIPAL=true`. Outbound calls to the `openai` embedding provider carry only
`traceparent`; inbound W3C `baggage` is never forwarded to it.

[`docs/otel-collector.yaml`](docs/otel-collector.yaml) is a minimal OpenTelemetry Collector
configuration that receives dolmen's traces and metrics:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 OTEL_SERVICE_NAME=dolmen-dev ./dolmen
```

## Rotating the secret key

1. Generate a new key: `openssl rand -base64 32`.
2. Restart dolmen with the new key in `DOLMEN_SECRET_KEY` and the old one added to
   `DOLMEN_SECRET_KEYS_OLD` (comma-separated; or `DOLMEN_SECRET_KEYS_OLD_FILE`, one per line).
   New writes use the new key at once; values written earlier still decrypt with the old one.
3. Call `rotate_secret_key` (needs `admin` on `*`) until it answers `"done": true`. Each call
   re-encrypts at most `limit` values (default 10000) in short transactions, bounded by half the
   operation timeout, so writers are not blocked for long. It is idempotent: after a crash or a
   timeout, call it again and it continues where it stopped.
4. Check `keys` in the last response: once the old key id shows 0 values (on a call without
   `namespace`), remove it from `DOLMEN_SECRET_KEYS_OLD` and restart.

Removing a key too early does not lose data as long as you still have it: reveals of values under
it answer `internal_error` (the log names the missing key id) and `rotate_secret_key` answers
`conflict` naming it; add it back and rerun. Idempotent inserts made before the rotation still
replay while the old key is configured; after it is removed, such a replay answers `conflict`
rather than inserting again.

## Backup and restore

A namespace is a SQLite file in WAL mode, so copying `<ns>.db` while the server runs can miss
commits still in the `-wal` file. Back up with the binary instead, which takes a consistent
snapshot of every namespace, and of the grant registry when auth is on, while writes continue:

```bash
dolmen backup -data ./data -out ./backups/2026-09-23
```

The backup directory must be new or empty. It gets one file per namespace, laid out as in the
data directory, and a `manifest.json`, written last, that records each file's size, SHA-256 and
catalog version. A backup interrupted before its manifest is written is never mistaken for a
finished one.

```bash
dolmen restore -from ./backups/2026-09-23 -data ./data
```

Restore checks every file against its manifest, runs SQLite's integrity check, and confirms this
binary can read each catalog before it writes anything. It then moves each file into place
atomically. It never overwrites: a namespace that already exists, or is open in a running
server, is refused. Restore into an empty data directory, or drop the namespace first. A file
already in place with the backed-up content is skipped, so a restore interrupted partway can
simply be run again.

A backup holds the data as of when it ran, so anything written afterwards is lost on restore. Run
backups as often as you can afford to lose writes, and keep them off the machine that holds
`-data`. The embedding model cache under `<data>/models` is not backed up, since it downloads
again. On `-engine postgres` both commands refuse: back up with `pg_dump`, as
[Running dolmen on PostgreSQL](docs/postgresql-operations.md) describes.

## Platform and filesystem support

Dolmen uses [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite), a pure-Go SQLite driver, so
no CGO is required.

| Concern | Policy |
|---|---|
| Operating systems | Linux, macOS, and Windows are supported. |
| Filesystem | Local filesystems (ext4, APFS, NTFS, etc.) are required. SQLite WAL uses shared-memory coordination that does not work reliably over network or shared filesystems (NFS, SMB); these are unsupported. The server detects NFS, SMB/CIFS, AFP, WebDAV, FUSE, 9p and cluster filesystems (and Windows network drives) at startup and logs a warning naming the filesystem. |
| WAL | Enabled per namespace (`journal_mode=WAL`, with `synchronous` set by `-sync`: `FULL` by default, `NORMAL` when opted out). Expect `<ns>.db`, `<ns>.db-wal`, and `<ns>.db-shm` files. |
| Permissions | On Unix the data directory is created `0700` and namespace `.db`/`-wal`/`-shm` files are set `0600` (owner only); on Windows `os.Chmod` only toggles the read-only attribute, so use NTFS ACLs for owner-only isolation. Startup refuses a data directory it cannot secure or write to (for example a read-only mount), naming the directory and the fix. |
| Locking | Each namespace has one writer connection (`MaxOpenConns=1`) with `BEGIN IMMEDIATE` locking, plus a separate read-only connection pool. WAL mode allows multiple concurrent readers, but only one writer per file at a time. |
| Multi-process | SQLite's file locking makes concurrent processes safe in principle, but running two dolmen servers against the same data directory can cause `database is locked` errors and is not recommended. |
| Deleting a namespace | Prefer `drop_namespace` (confirm-guarded, closes the server's own connections first). Manually: stop the dolmen process, then delete the three `<ns>.db*` files. |

## Not yet (deliberately)

Quotas, replication, time travel, compaction, a UI, webhooks, an Iceberg adapter. The MVP exists
to validate the tool surface with real agents.

## Development

```bash
make test            # go vet + go test ./...
make race            # go vet + go test -race ./...
make build           # static binary
make run             # run on :8790 with ./data
make vulncheck       # govulncheck ./...
make release         # cross-compile release binaries into dist/
make release-sbom    # generate an SPDX SBOM for the source tree
make release-checksums  # generate SHA256SUMS for dist/
make release-all     # release binaries + SBOM + SHA256SUMS
make image           # build a local container image
```

### PostgreSQL development status

The PostgreSQL backend is under development. It is selectable from the binary with
`-engine postgres -pg-dsn <dsn>` and from the Go API with `postgres.With` from
`github.com/lsm/dolmen/postgres`; SQLite remains the default on both. The implementation
includes pooled connections, namespace and table lifecycle, schema metadata, transaction
locking, native full-text search, and cross-process subscriptions, with PostgreSQL-backed
CI tests. See [Running dolmen on PostgreSQL](docs/postgresql-operations.md) for
requirements, role provisioning, catalog layout, and operational notes, and the
[implementation plan](docs/design/postgresql.md) for the conformance matrix and test
instructions.
Full-text search uses each backend's native ranking: SQLite keeps FTS5
BM25, while PostgreSQL uses its native text-search index and ranking. Relevance and
linguistic matching may differ when moving datasets between backends.


### Releasing

Pushing a `vX.Y.Z` tag starts the `release` workflow:

```bash
git tag v0.3.0
git push origin v0.3.0
```

Bump the fallback in `internal/version/version.go` when the release line changes. Release binaries
and the container image take their version from the tag via `-ldflags`; that constant is what a
plain `go build` or `go install` reports, so leaving it stale makes source builds misreport
themselves in `-version`, `GET /version`, MCP `serverInfo`, and the served-skill ETag.

The workflow creates a GitHub Release with static binaries for `linux/darwin/windows` on `amd64/arm64`, a `SHA256SUMS` file, and an SPDX SBOM. It also builds and pushes a multi-arch (linux/amd64 and linux/arm64) container image to `ghcr.io/lsm/dolmen`:

```bash
docker run --rm -it -p 127.0.0.1:8790:8790 -v dolmen-data:/data ghcr.io/lsm/dolmen:v0.3.0
```

The image contains a single static Go binary in a `gcr.io/distroless/static` base. `/data` is exposed as a volume and is created by the container if not mounted. The default container command binds to `0.0.0.0:8790` so the port can be published from Docker. Auth is off unless you set `DOLMEN_AUTH=on`, so keep the published port on a private network until you do; the [deployment guide](docs/deployment.md) covers running it for a team.

### Verifying artifacts

```bash
# Check a downloaded binary (skips entries for artifacts you didn't download)
sha256sum -c --ignore-missing SHA256SUMS

# Check the container image digest
oras manifest fetch ghcr.io/lsm/dolmen:v0.3.0
```

The `internal/conformance` package is the contract-conformance suite: black-box
tests over the HTTP and MCP transports that pin the interface — transport
parity between `/v1` and MCP `tools/call`, the golden error contract, the
limits table, typed-read coercion, write semantics, search invariants, and
migration guards. Behavior-changing PRs update the suite in the same PR.

## License

Apache-2.0 — see [LICENSE](LICENSE).
