---
name: dolmen
description: Persistent structured storage for this agent — tables with schema, full-text search, and vector search via a Dolmen server. Use whenever the task needs durable data across sessions: findings, memory, metrics, logs, or recall of earlier work. Not for ephemeral state.
---

# Dolmen — durable agent data

A Dolmen server exposes eighteen tools over MCP. Everything lives in namespaces (isolated databases);
pick one namespace per project or user and stay in it.

## Setup

### Run a server

The binary is at `github.com/lsm/dolmen`. Build and start it from a Dolmen checkout:

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

By default it listens on `127.0.0.1:8790` with no authentication, so keep it on a private
interface. On Unix the data directory is opened with owner-only permissions (`0700` for the
 directory, `0600` for files); on Windows use NTFS ACLs for owner-only isolation.

### Health check

Bash (honors `DOLMEN_URL` when set; a trailing slash is trimmed):

```bash
base="${DOLMEN_URL:-http://127.0.0.1:8790}"
curl -s "${base%/}/healthz"
```

Windows PowerShell (use `curl.exe` because `curl` is an alias for `Invoke-WebRequest`):

```powershell
$base = if ($env:DOLMEN_URL) { $env:DOLMEN_URL } else { "http://127.0.0.1:8790" }
curl.exe -s "$($base.TrimEnd('/'))/healthz"
```

Should return `{"status":"ok"}`. If the server is not running, do not improvise — start it first.

### Connect the skill

Add the MCP server to Claude:

Bash:

```bash
base="${DOLMEN_URL:-http://127.0.0.1:8790}"
claude mcp add --transport http dolmen "${base%/}/mcp"
```

Windows PowerShell:

```powershell
$base = if ($env:DOLMEN_URL) { $env:DOLMEN_URL } else { "http://127.0.0.1:8790" }
claude mcp add --transport http dolmen "$($base.TrimEnd('/'))/mcp"
```

The `dolmen` tools then appear in `tools/list` with full input schemas. The endpoint can also be
read from the environment: `DOLMEN_URL` (default `http://127.0.0.1:8790`).

If your workspace supports project skills, copy this file to `.claude/skills/dolmen/SKILL.md`.

If the `dolmen` MCP tools are not connected, do not improvise — ask the user to re-run the
connection command above.

## Working rules

1. **Check before creating.** Call `list_namespaces` then `list_tables` first; reuse an existing
   namespace or table when one fits. Only create tables for genuinely new kinds of data.
2. **Inspect when the schema is unknown or may have changed.** Call `describe_table` to get its
   schema, version, and row count. Use that to build correct `query` / `search_fulltext` /
   `search_vector` calls and to avoid inventing field names. Avoid calling it before every read or
   write on large tables — it runs a full `count(*)`, so cache the schema for the session.
3. **Prefer `infer_schema` → review → `create_table`.** Never invent a schema blind when sample
   records exist. Note: inference proposes plain types only — during review, mark the main text
   field `vectorize: true` yourself if you want semantic recall (requires an embedding provider
   on the server). Keep tables small and purposeful — a sprawl of near-duplicate tables is a
   failure mode.
4. **Record as you go.** After finishing a meaningful unit of work, `insert` a record summarizing it
   (what/where/outcome). Future sessions recall it via search.
5. **Read with the cheapest tool that answers the question:** `describe_table` → exact lookups via
   `query` (SQL, read-only) → `search_fulltext` for keyword recall → `search_vector` for
   meaning-based recall.
6. **Never write SQL that mutates.** `query` rejects it by design; use `insert`/`upsert_by_key`/`update`/`upsert`/`delete`/`migrate`.
7. **Evolve, don't fork.** When a table is missing a field, use `migrate` (add_field, rename_field,
   set_fulltext, set_vectorize) — do not create a parallel v2 table.

## Quick reference

- Schema types: `string`, `text` (long, searchable), `number`, `boolean`, `timestamp`, `json`,
  and `vector` (caller-supplied embeddings; requires a separate `"dim": N` property on the field).
- Field annotations: `fulltext: true` (FTS5 search), `vectorize: true` (server embeds this field —
  enables `search_vector` with `text`), `required: true`.
- `query` parameters: use `?` placeholders and pass `args` — never interpolate values into SQL.
- `delete` requires a `filter` (SQL WHERE expression); use `"1=1"` only when you truly mean everything.
- `drop_table` / `drop_namespace` are irreversible deletions (rows, search indexes, schema, history);
  both require `confirm` to repeat the exact name being dropped. Prefer `delete` unless the table or
  namespace itself must go.
- `update`/`upsert` take the same `filter` plus a `set` object of field values; all matched rows get
  the same values, and `set` to `null` clears an optional field. Matched updates accept partial `set`
  maps (missing required fields are not required), but `set` to `null` for a required field is
  rejected. Only the insert branch of `upsert` (or `upsert_by_key` for an unmatched record) enforces
  all required fields. Indexes and embeddings stay consistent automatically. `upsert` inserts `set`
  as one new record when the filter matches nothing — the idempotent way to keep one row per key.
- Every table has implicit `id` and `created_at` columns; `SELECT *` includes them.
- Retried writes must not duplicate rows: pass `idempotency_key` (any unique string) to `insert`, or use `upsert_by_key` with `"on": [field, ...]` naming the record's natural key (e.g. email, url) when the data identifies itself.
- Results honor declared field types in every read (`query`, `search_fulltext`, `search_vector`):
  `boolean` → `true`/`false`, `json` → the decoded value, `vector` → a number array, SQL `NULL` →
  `null`. In `query`, coercion is by result-column label (aliases count as their label); labels that
  match no declared field fall back to raw values (blobs as base64).
- The hidden `_embedding` column (from `vectorize`) is excluded from `SELECT *` and search results;
  reference it in the SQL (outside string literals and comments) or pass `include_hidden: true` to a
  search when you really need it.
- Vector search results carry `_score` (cosine similarity; higher is closer).
- `search_vector` has two query forms with different reach: `text` (server embeds it) searches only
  the vectorize `_embedding` space — a table without a `vectorize` field rejects `text`; `vector`
  (raw numbers) searches any `vector` column, and only you know which embedding space produced both
  the stored and the query vectors, so keep them from the same model.
- `skipped_vectors` in a `search_vector` response counts stored vectors that were corrupt or
  dimension-mismatched and could not be scored; nonzero means those rows are missing from results.

## Agent-critical caveats

### Quoting and placeholders

- `query` only accepts read-only `SELECT`/`WITH` statements. Bind all values with `?` and pass them in
  `args`. Identifiers and table names cannot be bound with `?`; write them directly from
  `list_tables`/`describe_table` and never let untrusted input choose them. `query` only checks that
  the statement is read-only, not that the identifiers are safe.
- SQL string literals use single quotes (`'value'`), escaped by doubling (`'can''t'`). Prefer `?`.
- Double quotes are for SQL identifiers, not string values.
- `search_fulltext` takes a raw FTS5 `MATCH` expression in `query`; it is **not** SQL, so do not wrap
  the whole expression in single quotes.

### Full-text (FTS5) search syntax

Dolmen indexes `fulltext` fields with SQLite FTS5 using the default `unicode61` tokenizer:
case-insensitive, diacritic-insensitive for most Latin characters (some non-Latin or multi-diacritic
characters may not normalize), no stemming. Most punctuation (including hyphens) is a token boundary.

- `payment` — one token.
- `payment gateway` — implicit `AND`.
- `payment OR gateway`.
- `payment NOT gateway`.
- `title:payment` — only in the `title` fulltext field.
- `{title body}:payment` — any of those fields.
- `"foo bar"` — phrase (adjacent tokens). Phrases match token adjacency, not literal punctuation.
- `"foo-bar"` — double-quote terms that contain spaces or punctuation; bare `foo-bar` is parsed as
  multiple terms and usually errors.
- `pay*` — prefix match.
- `NEAR(payment refund)` — proximity search (default near span). Use the group form
  `NEAR(term1 term2 ...)`; `term1 NEAR(term2)` is parsed as an implicit `AND` and does not enforce
  proximity.
- Terms like `"can't"` must be in double quotes; bare single quotes are a syntax error.

Results are ordered by FTS5 `rank` (BM25 by default): more relevant rows have a lower — more
negative — value and are returned first. The rank value itself is not returned.

### Vectors and semantic recall

- `vector` fields accept JSON number arrays of the declared `dim`; stored as float32 blobs, returned
  as `[]float64`.
- `vectorize: true` on a string/text field stores one embedding per non-empty row in `_embedding`.
  Only one field per table can be vectorized; rows with `null`, empty string, or missing values have
  `_embedding` NULL and are excluded from vector search.
- `search_vector(text=...)` embeds the query `text` with the configured provider and searches only
  the vectorize `_embedding` space — a table without a `vectorize` field rejects `text`.
  `search_vector(vector=[...])` supplies a query vector directly and may search any vector column.
- `column` applies to `vector` queries: it names the stored-vectors column and defaults to
  `_embedding` (if a vectorized field exists) or the first declared `vector` field. The query and
  stored vectors must come from the same embedding space. For `_embedding` (from `vectorize`) this
  means the same provider/identity; for caller-supplied `vector` fields it means the same model used
  for the stored and query vectors.
- Each result has `_score`: cosine similarity, higher is closer, typically `0`–`1` for positive
  embeddings (mathematically `-1`–`1`).
- `_embedding` is hidden from `SELECT *` and search results unless referenced explicitly or
  `include_hidden: true`.

### Id, `created_at`, and stability

- Every row has `id` and `created_at`. You cannot supply them; they are assigned on insert and
  returned in reads.
- `id` is `AUTOINCREMENT` — monotonically increasing and never reused after deletes — so it is safe
  to key off across sessions.
- `created_at` is a UTC millisecond ISO string, e.g. `2026-09-03T12:34:56.123Z`. Use string
  comparisons or SQLite date/time functions.

### Limits and guardrails

| Resource | Limit | Behavior |
|---|---|---|
| Namespace name | `^[a-z0-9][a-z0-9_-]{0,63}$` (max 64 chars) | rejected |
| Table / field name | `^[a-z][a-z0-9_]{0,63}$` (max 64 chars); reserved names (`id`, `created_at`, `_embedding`, `_score`, `_rank`, `rowid`) are rejected, and a field named `rank` is rejected when `fulltext: true` (reserved by the FTS5 index); table also cannot contain `__fts` or start with `sqlite_` | rejected |
| Table fields | 100 user-defined fields (not counting the implicit `id`, `created_at`, `_embedding` columns) | rejected |
| Records per `insert` / `upsert_by_key` | 1,000 | rejected |
| Natural key fields per `upsert_by_key` | 8 | rejected |
| Idempotency key length | up to 256 bytes; use printable ASCII | keys over 256 bytes are rejected; the JSON Schema enforces ASCII for schema-validating clients |
| Vector dimension (declared `vector` fields) | 1–4096 | rejected |
| Search `limit` | default 10, max 200 | omit `limit` for the default of 10; the tool schema enforces 1–200 for schema-validating clients, and the server clamps values above 200 to 200 (0 or negative selects the default on direct `/v1` calls) |
| `query` result rows | 1,000 | truncated with `truncated: true` |
| `query` / search result size | 32 MiB | first row over budget errors; later rows truncate; a single BLOB value over 32 MiB always errors |
| Request body | 32 MiB | rejected |
| `query` `args` | 100 | rejected |
| `infer_schema` samples | 1–50 | rejected |

Vector search is brute-force (fine into the low millions of rows); FTS5 uses an inverted index and
is much faster.

Validation notes:

- `number` becomes `int64` or `float64`: integral values within the int64 range become `int64`;
  unsigned Go values > `MaxInt64` are rejected, and integral JSON numbers outside the int64 range
  become `float64` (precision loss).
- `timestamp` must be a parseable ISO/RFC3339 string.
- `vector` must be a number array of exactly the declared `dim`; `NaN`/`Inf` are rejected. The
  4096-dimension cap applies only to declared `vector` fields; `vectorize` records the provider's
  returned dimension.
- Unknown field keys are rejected. Missing or `null` required fields are rejected on `insert` and on
  the insert branch of `upsert`/`upsert_by_key`.
- Namespace and table names are trimmed and lowercased before validation, so `"namespace":" Production "`
  operates on `production`.
- `query` accepts only `SELECT`/`WITH`, rejects embedded semicolons (trailing semicolons are accepted),
  and binds at most 100 `args`.
- `search_vector` with `text` requires a provider and searches only the server-managed `_embedding`
  column produced by a `vectorize: true` field — the provider identity must match the one that
  embedded the table, and a `text` query naming a declared `vector` column is rejected. Searches
  with a caller-supplied `vector` need no provider and are not checked against any embedding
  space — only you know which model produced the stored and query vectors.
- `insert` with an `idempotency_key`: the same key + same records replays the original ids; the same
  key with different records is rejected. Use printable ASCII keys (`[ -~]`) up to 256 bytes.

## Typical flows

Store session findings:

```
list_tables(namespace="research")                       → review names →
describe_table(namespace="research", table="findings")
  → missing:
    infer_schema(samples=[{...one finding...}])         → review proposal →
    create_table(namespace="research", table="findings", fields=[...])
    insert(namespace="research", table="findings", records=[{...}])
  → exists but wrong shape:
    use migrate for the supported changes (add_field/rename_field/drop_field/set_fulltext/
    set_vectorize) — do not create a v2 table. If the mismatch is outside those operations
    (e.g., changing a field type or adding a required field to a populated table), stop and ask
    the user before rebuilding or backfilling.
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
