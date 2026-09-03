---
name: dolmen
description: Persistent structured storage for this agent — tables with schema, full-text search, and vector search via a Dolmen server. Use whenever the task needs durable data across sessions: findings, memory, metrics, logs, or recall of earlier work. Not for ephemeral state.
---

# Dolmen — durable agent data

A Dolmen server exposes ten tools over MCP. Everything lives in namespaces (isolated databases);
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
5. **Never write SQL that mutates.** `query` rejects it by design; use `insert`/`delete`/`migrate`.
6. **Evolve, don't fork.** When a table is missing a field, use `migrate` (add_field, rename_field,
   set_fulltext, set_vectorize) — do not create a parallel v2 table.

## Quick reference

- Schema types: `string`, `text` (long, searchable), `number`, `boolean`, `timestamp`, `json`,
  and `vector` (caller-supplied embeddings; requires a separate `"dim": N` property on the field).
- Field annotations: `fulltext: true` (FTS5 search), `vectorize: true` (server embeds this field —
  enables `search_vector` with `text`), `required: true`.
- `query` parameters: use `?` placeholders and pass `args` — never interpolate values into SQL.
- `delete` requires a `filter` (SQL WHERE expression); use `"1=1"` only when you truly mean everything.
- Every table has implicit `id` and `created_at` columns; `SELECT *` includes them.
- Results honor declared field types in every read (`query`, `search_fulltext`, `search_vector`):
  `boolean` → `true`/`false`, `json` → the decoded value, `vector` → a number array, SQL `NULL` →
  `null`. In `query`, coercion is by column name; computed expression columns fall back to raw
  values (blobs as base64).
- The hidden `_embedding` column (from `vectorize`) is excluded from `SELECT *` and search results;
  select it explicitly in SQL or pass `include_hidden: true` to a search when you really need it.
- Vector search results carry `_score` (cosine similarity; higher is closer).

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
