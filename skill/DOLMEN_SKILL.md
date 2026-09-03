---
name: dolmen
description: Persistent structured storage for this agent — tables with schema, full-text search, and vector search via a Dolmen server. Use whenever the task needs durable data across sessions: findings, memory, metrics, logs, or recall of earlier work. Not for ephemeral state.
---

# Dolmen — durable agent data

A Dolmen server exposes thirteen tools over MCP. Everything lives in namespaces (isolated databases);
pick one namespace per project or user and stay in it.

## Setup

The server endpoint comes from the environment: `DOLMEN_URL` (default `http://127.0.0.1:8790`).
If the `dolmen` MCP tools are not connected, do not improvise — ask the user to run
`claude mcp add --transport http dolmen ${DOLMEN_URL:-http://127.0.0.1:8790}/mcp`.

## Working rules

1. **Check before creating.** Call `list_tables` first; reuse an existing table when one fits.
   Only create tables for genuinely new kinds of data.
2. **Prefer `infer_schema` → review → `create_table`.** Never invent a schema blind when sample
   records exist. Note: inference proposes plain types only — during review, mark the main text
   field `vectorize: true` yourself if you want semantic recall (requires an embedding provider
   on the server). Keep tables small and purposeful — a sprawl of near-duplicate tables is a
   failure mode.
3. **Record as you go.** After finishing a meaningful unit of work, `insert` a record summarizing it
   (what/where/outcome). Future sessions recall it via search.
4. **Read with the cheapest tool that answers the question:** `describe_table` → exact lookups via
   `query` (SQL, read-only) → `search_fulltext` for keyword recall → `search_vector` for
   meaning-based recall.
5. **Never write SQL that mutates.** `query` rejects it by design; use `insert`/`upsert_by_key`/`update`/`upsert`/`delete`/`migrate`.
6. **Evolve, don't fork.** When a table is missing a field, use `migrate` (add_field, rename_field,
   set_fulltext, set_vectorize) — do not create a parallel v2 table.

## Quick reference

- Schema types: `string`, `text` (long, searchable), `number`, `boolean`, `timestamp`, `json`,
  and `vector` (caller-supplied embeddings; requires a separate `"dim": N` property on the field).
- Field annotations: `fulltext: true` (FTS5 search), `vectorize: true` (server embeds this field —
  enables `search_vector` with `text`), `required: true`.
- `query` parameters: use `?` placeholders and pass `args` — never interpolate values into SQL.
- `delete` requires a `filter` (SQL WHERE expression); use `"1=1"` only when you truly mean everything.
- `update`/`upsert` take the same `filter` plus a `set` object of field values; all matched rows get
  the same values, and `set` to `null` clears a field. Indexes and embeddings stay consistent
  automatically. `upsert` inserts `set` as one new record when the filter matches nothing (it must
  then satisfy required fields) — the idempotent way to keep one row per key.
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
- `title:payment` — only the `title` fulltext field.
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
- `search_vector(text=...)` embeds the query `text` with the configured provider and compares it
  against the resolved `column`. `search_vector(vector=[...])` supplies a query vector directly; `column`
  still selects the searched stored vectors.
- `column` defaults to `_embedding` (if a vectorized field exists) or the first declared `vector` field.
  The query and stored vectors must come from the same embedding space. For `_embedding` (from
  `vectorize`) this means the same provider/identity; for caller-supplied `vector` fields it means the
  same model used for the stored and query vectors.
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

### Limits and performance

- `query` caps at 1000 rows and 32 MiB; `truncated: true` if more rows exist.
- `search_fulltext` and `search_vector` default to 10, max 200, 32 MiB budget.
- `insert` up to 1000 records per call; chunk larger batches.
- `create_table` up to 100 user fields.
- Vector search is brute-force; fine for low millions. FTS5 uses an index and is fast.

## Typical flows

Store session findings:

```
describe_table(namespace="research", table="findings")   → missing →
infer_schema(samples=[{...one finding...}])               → review proposal →
create_table(namespace="research", table="findings", fields=[...])
insert(namespace="research", table="findings", records=[{...}])
```

Recall in a later session:

```
search_fulltext(namespace="research", table="findings", query="auth")   # needs a fulltext field;
                                                                       # search_vector with text needs a
                                                                       # vectorize field plus a provider
query(namespace="research", sql="SELECT * FROM findings WHERE created_at >= ? ORDER BY created_at DESC", args=["2026-09-01"])
```
