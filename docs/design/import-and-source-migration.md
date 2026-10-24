# Identity-preserving import and source migration

Approved by the human for #704 at `aed68c4`; implementation tracked by #705 under
#698. This is a narrow exception to D20 in
[identity-and-engines.md](identity-and-engines.md): importing application rows now
has a demander. General export, replication, and backup/restore remain unchanged;
The D20 decision index and implementation plan link to this approved exception.

## Import contract

Add a separate `import_records` HTTP/MCP operation and Go `Store.ImportRecords`
method, not an option on ordinary insert/upsert or a new Batch kind. Input names
one existing namespace/table, 1–1,000 records and a required idempotency key;
existing request-size, timeout and field limits apply. Each record must include
`id` (an exact positive int64 below MaxInt64) and `created_at` in the existing
canonical UTC-millisecond form `YYYY-MM-DDTHH:MM:SS.sssZ` (four-digit year).
Preserve the supplied ID and canonical timestamp exactly; never round IDs through
float64. Reject noncanonical timestamps, missing metadata, duplicate IDs, unknown
fields and invalid values before committing. Fixed-width UTC timestamps retain
the existing chronological text-ordering contract. Normal writes continue to reject
supplied system columns. No persistent “import mode” switch survives the request.

Example after explicitly creating an `events` table with a text `body` field:

```json
{"namespace":"demo","table":"events","idempotency_key":"snapshot-a-events-0001",
 "records":[{"id":42,"created_at":"2026-01-02T03:04:05.123Z","body":"sample"}]}
```

Import never overwrites: an existing ID with identical content is a no-op, counted
as `skipped`; only different content at that ID is `conflict`. Compare canonical
`created_at` and all validated, typed user fields (including nulls/defaults), not
JSON key order or derived indexes/embeddings. Compare and insert under the same
transaction, so retries also work without the original chunk boundaries or key.
One chunk is atomic, including rows, indexes, change-feed records and replay receipt;
any conflict rolls back its new rows. Skips emit no change events. Identical
key/payload retries return the original receipt with `replayed: true`; a changed
payload with that key conflicts. Reuse existing table incarnation and idempotency
machinery, with an import-specific receipt domain. Return ordered `ids`, `inserted`,
`skipped`, `replayed` and `changes`; a replay receipt retains its original counts.
For deterministic retries, omitted timestamp defaults of `now` use the imported
`created_at`; export explicit values to preserve a different source default.

SQLite, Postgres and lakehouse targets share this contract. Reserve the allocator
above both its previous high-water mark and the greatest imported ID before
acknowledgement, under the same write coordination as ordinary inserts. Postgres
sequence advances may leave gaps on rollback, but must never move backwards or
race an ordinary allocator into a collision. Imported IDs cannot reach MaxInt64,
so a following ordinary insert has representable space. Tests cover concurrent
normal writes and later imports below the high-water mark. Reuse ordinary field,
secret, file and embedding validation; rebuild derived search indexes rather than
copying internal tables. Under auth-on require table `admin` and `create`, without
row scope; never import grants, owner identities, secrets keys or internal metadata.

## SQLite and Postgres migration path

Ship a streaming `dolmen import` JSONL client plus executable, synthetic export
recipes for `sqlite3` and `psql`. It reads stdin and submits bounded chunks to
`import_records`; no source database connection or SQL dump execution in dolmen.
Explicit schema/name/type mapping precedes import using existing table APIs.
Export application tables only from a consistent read-only snapshot (SQLite read
transaction; Postgres repeatable-read read-only transaction). Quiesce source writes
through validation and cutover; live change capture is outside this work.

Recipes emit one mapped record per line, ordered by source ID. Native JSON builders
preserve integer tokens, nulls and escaped text; the client decodes with UseNumber.
Require a positive integral source key; UUID/composite/nonpositive keys need an
explicit external mapping retaining the original key in a user field. Canonical
creation strings pass through unchanged. Export recipes explicitly convert other
timestamps to UTC and truncate fractional seconds to milliseconds (never round),
so Postgres microseconds migrate rather than fail. The export report counts rows
whose instant lost sub-millisecond precision, per table and in total; e.g.
`.123456Z` becomes `.123Z` and increments the count, while `.123000Z` does not.
Retain the original timestamp in an explicitly mapped user field when exact source
precision is needed. Dolmen still accepts only canonical milliseconds and never
truncates on import. Missing or ambiguous times, unsupported types and lossy numeric
conversions fail with row/field location,
without echoing values or connection strings. Map binary/file data through
existing file uploads, not source paths; rebuild embeddings with the target model.
Do not silently infer schema, truncate decimals, or transfer SQL constraints/triggers.

The client bounds chunks by rows and encoded request bytes, reports committed chunk
numbers and stops on the first error. Stable snapshot name/table/chunk keys allow
restart from the same immutable export; regrouped or changed payloads require new
keys. Identical existing rows are skipped; changed existing rows conflict. Earlier
chunks remain committed; there is no cross-table transaction.
Validate counts, ordered IDs, explicitly truncated timestamps, mapped values and references
before switching readers. Keep source/export intact; failed migrations never clean
up tables or overwrite a previously populated target automatically.

## Delivery and proof

Slice 1 adds the internal SQLite import path and shared identity validation. Public
HTTP/MCP and Go facade exposure waits for slice 4; other engines follow in 2–3.
Slice 2 adds the same internal path on Postgres: explicit IDs go in with
`OVERRIDING SYSTEM VALUE`, the identity sequence is raised with `setval` to the
greater of its last value and the chunk's highest new ID under the namespace write
lock, and existing IDs are compared by reading a scratch copy of the candidate
rows (a `LIKE` temp table, dropped before commit) through the ordinary row decoder.
Slice 3 adds it on the lakehouse: imported IDs ride the ordinary append commit,
which raises `next_id` to past the highest of them under the store write lock;
existing IDs are compared by presenting both the stored row and the candidate
through the ordinary row presenter, with secrets opened from their catalog side table.

Small independently testable PRs: shared import validation and SQLite engine;
Postgres allocator/import; lakehouse import; HTTP/MCP and Go facade wiring;
JSONL client and SQLite/Postgres recipes with end-to-end verification. Each stage
is about 100–200 production lines; split further by working boundary if needed.
No changes to Batch CAS, constraints or benchmarking owned by parallel threads.
Conformance pins transport/facade parity, authorization, rollback, replay, large IDs,
canonical timestamp ordering, counted truncation, search and subsequent normal writes.
Synthetic SQLite and microsecond-bearing Postgres snapshots must migrate and compare
mapped rows. Retry acknowledged chunks and interrupted streams with both original
and new chunk keys; assert skipped counts, no duplicate events, differing-content
conflicts and rollback of new rows in a conflicting chunk. Document commands/results;
no private source data or credentials in examples, fixtures or logs.
