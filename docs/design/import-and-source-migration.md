# Identity-preserving import and source migration

Proposed for #704; implementation tracked by #705 under #698. Human approval is
required before implementation. This proposes a narrow exception to D20 in
[identity-and-engines.md](identity-and-engines.md): importing application rows now
has a demander. General export, replication, and backup/restore remain unchanged;
the first implementation PR updates that older portability decision.

## Import contract

Add a separate `import_records` HTTP/MCP operation and Go `Store.ImportRecords`
method, not an option on ordinary insert/upsert or a new Batch kind. Input names
one existing namespace/table, 1–1,000 records and a required idempotency key;
existing request-size, timeout and field limits apply. Each record must include
`id` (an exact positive int64 below MaxInt64) and `created_at` (a valid RFC3339
string). Preserve both values, including timestamp fractional precision; never
round IDs through float64. Reject missing metadata, duplicate IDs, unknown fields
and invalid values before committing. Normal writes continue to reject supplied
system columns. No persistent “import mode” switch survives the request.

Example after explicitly creating an `events` table with a text `body` field:

```json
{"namespace":"demo","table":"events","idempotency_key":"snapshot-a-events-0001",
 "records":[{"id":42,"created_at":"2026-01-02T03:04:05.123456Z","body":"sample"}]}
```

Import is insert-only: an occupied ID is `conflict`, even with identical contents;
never replace, merge or delete existing rows. One chunk is one atomic transaction,
including IDs, timestamps, user fields, indexes, change-feed records and replay
receipt. Identical key/payload retries return the original IDs without another
change event; a changed payload with that key conflicts. Reuse existing table
incarnation and idempotency machinery, with an import-specific receipt domain.
Return `ids`, `inserted`, `replayed` and `changes` using existing write shapes.

SQLite, Postgres and lakehouse targets share this contract. Reserve the allocator
above both its previous high-water mark and the greatest imported ID before
acknowledgement, under the same write coordination as ordinary inserts. Postgres
sequence advances may leave gaps on rollback, but must never move backwards or
race an ordinary allocator into a collision. Imported IDs cannot reach MaxInt64,
so a following ordinary insert has representable space. Tests cover concurrent
normal writes and later imports below the high-water mark. Reuse ordinary field,
secret, file and embedding validation; rebuild derived search indexes rather than
copying internal tables. Under auth-on require table `admin` and `insert`, without
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
explicit external mapping retaining the original key in a user field. Existing
RFC3339 creation strings pass through unchanged; other timestamps need an explicit
timezone/conversion preserving the instant and precision. Missing creation times,
ambiguous times, unsupported types and lossy numeric conversions fail with row/field
location, without echoing values or connection strings. Map binary/file data through
existing file uploads, not source paths; rebuild embeddings with the target model.
Do not silently infer schema, truncate decimals, or transfer SQL constraints/triggers.

The client bounds chunks by rows and encoded request bytes, reports committed chunk
numbers and stops on the first error. Stable snapshot name/table/chunk keys allow
restart from the same immutable export; changed exports require new keys and a fresh
target. Earlier chunks remain committed; there is no cross-table transaction.
Validate source/target counts, ordered IDs, timestamps, mapped values and references
before switching readers. Keep source/export intact; failed migrations never clean
up tables or overwrite a previously populated target automatically.

## Delivery and proof

Small independently testable PRs: shared import validation and SQLite engine;
Postgres allocator/import; lakehouse import; HTTP/MCP and Go facade wiring;
JSONL client and SQLite/Postgres recipes with end-to-end verification. Each stage
is about 100–200 production lines; split further by working boundary if needed.
No changes to Batch CAS, constraints or benchmarking owned by parallel threads.
Conformance pins transport/facade parity, authorization, rollback, replay, large IDs,
precision, search and subsequent normal writes. Synthetic SQLite and Postgres
snapshots must migrate into fresh targets and compare mapped rows, then retry an
acknowledged chunk and an interrupted stream. Document exact commands/results;
no private source data or credentials in examples, fixtures or logs.
