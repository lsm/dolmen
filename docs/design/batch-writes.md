# Batch writes — design note

**Status: settled, not implemented.** The ten decisions below are Marc's, the detail under each is
this note's, and the slice order at the end is what the implementing PRs follow. Nothing here is code.

The problem: an agent that has twelve related writes to make issues twelve calls, and on SQLite each
one takes the namespace's single writer, so twelve round trips of lock handoff where the caller
actually wants one. A batch is one call, one transaction, one change-feed commit.

---

## 1. Scope

**One namespace. Any mix of its tables.**

Not cross-namespace: SQLite's writer is per namespace file and PostgreSQL's transaction would have to
span schemas that are not co-located in any way the engine can assume. A caller needing two namespaces
makes two batches.

Not schema changes: `create_table`, `migrate` and `drop_*` keep their own semantics (a migration is
planned against a version, stamped, and retried on its own terms — see
[embedding-backfill.md](embedding-backfill.md) for what it costs to put one inside a writer). A batch
that changed a table's schema mid-flight would make every other write in the batch plan against a
schema it did not read. Out of scope, deliberately, not accidentally.

---

## 2. The surface

A new `batch` operation. Input:

```json
{
  "namespace": "research",
  "writes": [
    { "kind": "insert", "table": "findings", "records": [{"title": "…"}] },
    { "kind": "update", "table": "findings", "filter": "status = 'new'", "args": [], "set": {"status": "triaged"} },
    { "kind": "upsert_by_key", "table": "sources", "on": ["url"], "records": [{"url": "…", "title": "…"}] }
  ],
  "idempotency_key": "import-2026-09-28-01"
}
```

**Each write names its `kind` and otherwise carries exactly the input of the matching single operation.**
Five kinds: `insert`, `update`, `delete`, `upsert` (filter-matched), `upsert_by_key` (natural key).

Two mechanical consequences, both of which are the point:

- **`namespace` is hoisted.** The batch has one namespace; a per-write `namespace` would be either
  redundant or a way to contradict it. The per-write object is the single op's input schema **minus
  `namespace`**, with `kind` added.
- **The schemas are derived, not copied.** `api.Ops` stays the single source of truth: the `batch`
  input schema for kind *k* is `Ops[k].InputSchema`'s properties with `namespace` removed. A change to
  `insert`'s schema propagates to `batch` rather than needing to be made twice, and a divergence is
  not possible. This is the same discipline `outputSchemas` already relies on in `openapi.go`.

### Limits

Existing limits apply unchanged to each write: 1,000 records per `insert`, 1,000 ids per `read_rows`,
8 key fields per `upsert_by_key`, 100 `args` per filter or query, the delete match cap with `confirm`,
the 32 MiB request body, 100 user-defined fields per table.

Two are new, because a batch can otherwise be a way around limits that exist for a reason:

| limit | value | why that number |
| --- | --- | --- |
| writes per batch | 100 | Each write is at least one statement. 100 is enough that a real import is one call and small enough that a rejected batch is cheap to reason about and to print in an error. |
| rows touched per batch | 1,000 | **Summed over all writes**, and counted in *rows touched*: `insert` records, filter-matched update rows, matched delete rows, `upsert` inserts. This is the limit that does the real work — see §9. |

Reusing `MaxRecordsPerInsert` as the batch-wide total is the important part. A batch must not be a way
to write 50,000 rows where one `insert` is allowed 1,000; the total is the same 1,000, spent across
whichever writes need it.

A write that would push the batch past either cap is rejected before the transaction, naming the write
by index and the cap by name.

---

## 3. Atomicity

**One transaction on both engines.** SQLite: take the namespace writer once (`beginWrite`) and run
every write on that same `*sql.Tx`. PostgreSQL: one pgx transaction over the batch.

**The change feed records the whole batch as one commit.** This falls out of the transaction rather
than needing new code: the change log lives in the same SQLite file / the same PostgreSQL catalog, so
the records a batch writes are visible to `changes_since`, `wait_for` and `subscribe` exactly when the
transaction commits. No subscriber ever sees a prefix of a batch, and a rolled-back batch writes no
change records at all.

This is the reason the feature exists and the reason it is not negotiable into per-write commits: a
subscriber that saw half an import would have to reconcile, and the whole pitch of the change feed is
that it is gap-free and in commit order.

---

## 4. Idempotency

**One key for the whole batch**, at the top level, reusing `insert`'s existing mechanism (256 printable
ASCII bytes, omitted for a non-idempotent batch).

- A replay with the same key and the same body returns the **stored result** — the whole per-write
  result array — and writes nothing, exactly as `insert` replays ids.
- The same key with a different body is a `conflict`, as with `insert`.
- **A write inside a batch may not carry its own `idempotency_key`.** Rejected as `invalid_request`
  naming the index. Two levels of idempotency would mean a replay of the outer batch could not tell
  whether an inner key had already been consumed by a different batch, and the stored result would not
  describe what actually happened.

The stored result is the batch's, so a replay is exact: the caller learns which ids each insert
returned, not merely that something committed.

---

## 5. Errors

**All or nothing. Nothing is written.**

The error names the failing write by index and keeps that write's own class:

```
writes[2]: unknown field "titel" on table findings (see describe_table)      invalid_request
writes[0]: no row matched the filter "url = '…'" (see describe_table)       invalid_request
writes[1]: table findings is not visible to this principal                    forbidden
```

So a `forbidden` inside a batch is a `forbidden` for the batch — the caller does not have to parse a
message to learn it was an authorization problem — and the prefix is the write's, not the batch's. The
prefix is assembled by the API layer from a validated index, so caller input cannot rewrite it.

Ordering matters and is part of the contract: **writes are validated in order and the first failure is
the one reported**, because a caller fixing a batch wants the earliest problem, not the set. A batch
that fails on write 3 has written nothing, so re-sending it after fixing write 3 is safe even without an
idempotency key.

---

## 6. Embeddings

**Computed before the transaction, as `insert` and `update` do**, then re-checked inside it.

Phase 1, outside the writer: read each table's schema, embed space and dimension; count what each
update and delete matches; embed every vectorized write's text through the provider. This is where a
slow or failing provider is absorbed — the writer is not held across a provider round trip, which is the
same property #501 and #509 established for the single operations.

Phase 2, inside the transaction: **re-check each table's version, embedding space, dimension and drop
generation**, and re-check the counts the plan was built on. If any moved, roll back and retry the whole
batch, bounded at three attempts (matching `updateOrUpsert`'s bound).

Retrying the whole batch rather than retrying the write is deliberate: a retry that re-planned one write
against a newer schema and committed it alongside writes planned against the older one would produce
exactly the mixed-state batch §3 exists to prevent.

Because each write goes through the same helper as its single operation, the in-writer fallback from
#509 comes along for free — the case where a write's own count grew between the pre-read and the writer
is handled by the code that already handles it, rather than by a batch-specific path that would have to
be written and tested separately.

---

## 7. Authorization

**Each write is checked against its own table's verbs and row scope, before anything is written.** A
forbidden write fails the whole batch, naming its index.

This is the one place `batch` cannot use the existing `authRules` shape. `internal/api/authz.go` maps an
operation name to one verb set and one scope; `batch` is not one operation, it is up to a hundred, each
against a different table with possibly a different `row_access`. So the `batch` rule is a dispatcher
into the four existing rules:

| write kind | verbs | scope |
| --- | --- | --- |
| `insert` | `create` | the write's table |
| `update` | `update` | the write's table |
| `delete` | `delete` | the write's table |
| `upsert`, `upsert_by_key` | `create` + `update` | the write's table |

with `RowScope` resolved per table, because `row_access: own` stamps an owner column per table and a
batch spanning two such tables must stamp each correctly.

**All checks run before the transaction.** A batch that would be refused on write 7 must not have taken
the writer to find that out on write 7. This is the same reasoning as the limits in §2.

A caller with `create` on one table and nothing on another can batch the first and is refused the whole
batch on the second — which is the correct reading of "all or nothing", and is worth stating plainly
because it is more restrictive than per-write commits would be. A caller who needs a partial success
should issue two batches.

---

## 8. Results

One result per write, in order, positionally aligned with `writes` so a caller can zip the two without
matching on anything:

- `insert` → `ids`, plus `inserted`
- `update` → `updated`
- `delete` → `deleted`, plus `matched` when a `dry_run` is in play (§11)
- `upsert`, `upsert_by_key` → `inserted`, `updated`, `ids`

This is the union of what the single operations already return, so nothing here is a new result shape —
only a place to put them. The output schema is derived the same way the input is, per kind.

---

## 9. Writer hold time on SQLite

**The batch holds the one writer for all its writes.** That is the cost, and it is why the limits in §2
are row-based rather than statement-based.

What bounds it:

- **1,000 rows touched**, which is the work an `insert` is already allowed to do in one transaction.
- **100 writes**, which bounds the per-statement overhead — schema lookups, temp tables for the
  filter-matched writes, change-log records — that a write costs even when it touches no rows.
- **The operation timeout** (`-op-timeout`, 2 minutes by default), after which the transaction is
  rolled back and the batch is reported as `timeout` like any other operation. Note this is a *server*
  limit: a batch that trips it is rolled back whole, so a caller cannot make it not-trip by trying
  again, only by sending less.

What it does not bound, and this is the honest part: the provider. Embeddings are computed before the
writer is taken (§6), so a slow model costs the caller latency but never blocks a concurrent writer.

Readers are unaffected throughout. WAL mode gives readers a snapshot that does not wait for the writer,
which is why `changes_since` and `wait_for` stay responsive while a large batch commits.

---

## 10. The Go library

**`Store.Batch` ships, in the last slice, with parity tests.**

The alternative was to record it as a divergence in
[facade-input-matrix.md](facade-input-matrix.md). The argument against shipping: the facade's surface
is deliberately a subset, and a batch is a convenience rather than a capability. The argument for
shipping, which won: the facade already exposes every other write operation
(`Insert`/`Update`/`Delete`/`UpsertByKey`), so a `batch` that exists only on the wire would be the one
write path an embedder could not reach. Divergences in that matrix are recorded because the surfaces
*should* differ — a typed-Go numeric edge case, an HTTP-vs-facade validation boundary. "We did not get
to it" is not a design position, and a matrix entry is the wrong place to park it.

Parity tests cover the same cases the wire gets: atomicity, the single feed commit, idempotent replay,
the embedding re-check, and a failing write leaving nothing behind.

---

## 11. Deliberately not in scope

- **`dry_run`.** Tempting and cheap for `insert` and `delete`, where the plan is a count. It is not
  cheap for `upsert`: deciding insert-versus-update without writing requires the plan to be taken
  inside the transaction, which is a different code path from the one that would ship. Getting that
  subtly wrong is worse than not having the operation, so it is the first candidate for a follow-up
  rather than a detail of this one.
- **Cross-namespace batches** (§1) and **schema changes inside a batch** (§1).
- **Per-write idempotency keys** (§4).
- **Partial success.** All-or-nothing is the contract; a caller who wants a best effort sends what
  must succeed and handles the rest itself.

---

## 12. Slices

Each is its own PR off `main`, opened after the previous merges. Failing test first, in every slice
that has code.

1. **This note.** No code.
2. **The engines.** `Batch` on SQLite and PostgreSQL, with engine tests for atomicity, the single feed
   commit, idempotent replay, the embedding re-check and a failing write leaving nothing behind.
3. **The wire.** The `batch` entry in `api.Ops` with derived schemas, its `toolAnnotations`, the
   per-write `authRules` dispatcher, the OpenAPI output, conformance over `/v1` and MCP on both
   engines, and `skill/dolmen.md` and the README.
4. **The Go library.** `Store.Batch` and parity tests (§10).

The limits in §2, the reuse rule in §2 and the index-prefixed errors in §5 are the three places where
the wire and the engine could drift apart if they are written twice. They are written once, in the
engine or in `api.Ops`, and everything else derives.
