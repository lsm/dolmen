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

And the result, positionally aligned with `writes` (§8):

```json
{
  "replayed": false,
  "results": [
    { "ids": [41], "inserted": 1 },
    { "updated": 7 },
    { "inserted": 0, "updated": 1, "ids": [12] }
  ]
}
```

**Each write names its `kind` and otherwise carries exactly the input of the matching single operation.**
Five kinds: `insert`, `update`, `delete`, `upsert` (filter-matched), `upsert_by_key` (natural key).

Two mechanical consequences, both of which are the point:

- **`namespace` is hoisted.** The batch has one namespace; a per-write `namespace` would be either
  redundant or a way to contradict it.
- **Three fields are removed from every derived schema, and the reason for each differs.** The per-write
  object is the single op's input schema with `namespace` removed, `kind` added, and the other two
  removals below applied. `namespace` because it is hoisted. `idempotency_key` because §4 refuses it —
  a batch has one key, and an inner one could not be replayed coherently. `dry_run` because §11 takes
  dry runs out of scope, and because a simulated write inside a committing batch has no defined meaning:
  under all-or-nothing, either every write commits or none does, and a write that reports what it *would*
  have done is neither. A caller sending `dry_run` on a `delete` write is refused as `invalid_request`
  naming the index, not silently ignored — which is what a per-write schema keeping
  `additionalProperties: false` gives for free, the same as any unknown field. `delete` is the only kind
  whose input has it today; the removal is stated as a rule so a future op that grows the field does not
  inherit it by accident.
- **The schemas are derived, not copied.** `api.Ops` stays the single source of truth: the `batch`
  input schema for kind *k* is `Ops[k].InputSchema`'s properties minus those three fields. A change to
  `insert`'s schema propagates to `batch` rather than needing to be made twice, and a divergence is
  not possible. The precedent already in the codebase is `writeOutSchema(withUpdated, withReplayed bool)`
  at `internal/api/ops.go:158`, which builds one output schema and parameterises it per kind — `insert`
  asks for `(false, true)`, `upsert` and `upsert_by_key` for `(true, false)` — so three operations share
  a builder instead of three literals. That is the discipline to copy.

  **The thing to avoid is the opposite pattern, and this repo already contains it:**
  `outputSchemas` in `internal/api/openapi.go` is a hand-maintained map whose `init()`
  (`openapi.go:82-88`) *replaces* the `OpDef.OutputSchema` of 17 operations at startup. For those,
  editing the literal in `ops.go` has no effect on what OpenAPI or MCP advertise — CLAUDE.md names it
  as the one override trap. A `batch` output schema added there would be a hand-written second copy of
  something the per-kind derivation already produces, and editing the derivation would silently not
  change what is advertised. So: derive it, and do not add `batch` to `outputSchemas`.

### Limits

Existing limits apply unchanged to each write: 1,000 records per `insert`, 1,000 ids per `read_rows`,
8 key fields per `upsert_by_key`, 100 `args` per filter or query, the delete match cap with `confirm`,
the 32 MiB request body, 100 user-defined fields per table.

Two are new, because a batch can otherwise be a way around limits that exist for a reason:

| limit | value | why that number |
| --- | --- | --- |
| writes per batch | 100 | Each write is at least one statement. 100 is enough that a real import is one call and small enough that a rejected batch is cheap to reason about and to print in an error. |
| rows touched per batch | 1,000 | **Summed over all writes**, and counted in *rows touched*, which is every row every write creates or matches: `insert` records, `upsert` inserts, `upsert_by_key` records, and the rows matched by `update`, `delete`, and the update branch of `upsert`. This is the limit that does the real work — see §9. |

Reusing `MaxRecordsPerInsert` as the batch-wide total is the important part. A batch must not be a way
to write 50,000 rows where one `insert` is allowed 1,000; the total is the same 1,000, spent across
whichever writes need it.

The counted set is deliberately every write kind and both branches of every kind that can match rows.
An earlier draft of this table listed `insert` records, matched `update` and `delete` rows and `upsert`
inserts, which would have let a batch of a hundred `upsert_by_key` writes touch 100,000 rows and a
matched `upsert` update touch an uncounted number — the cap defeated at the two kinds most likely to be
used in a loop. Counting what a write *touches* rather than what it *carries* is the property that makes
the number mean what §9 needs it to mean.

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

The caller is also told it was a replay, by a **top-level `replayed` boolean** on the batch result
(§8) — the batch's counterpart to the single ops' `replayed` field, moved up one level because the key
belongs to the batch. `insert` reports `replayed` today for exactly this reason, so a batch that
dropped the signal would leave a caller unable to distinguish a fresh commit from a recovered one.

---

## 5. Errors

**All or nothing. Nothing is written.**

The error names the failing write by index and keeps that write's own class. Every message below is
quoted from the code that produces it, because a batch that wrapped a plausible paraphrase would break
the one property callers rely on — that these strings are stable enough to test against:

```
writes[2]: unknown field "titel" on table findings (see describe_table)          invalid_request
writes[1]: filter matched 1200 rows, exceeding the delete limit of 1000; …        invalid_request
writes[0]: field "title" is required (no row matched the filter, so upsert
           would insert a new record)                                            invalid_request
writes[3]: the caller holds no grant permitting this operation on this object;
           an administrator grants access with the grant op, and whoami reports
           the principal and groups this request authenticated as             forbidden
```

From `internal/store/insert.go:117`, `internal/store/search.go:450`,
`internal/store/update.go:176` and `internal/api/authz.go:111`.

Two of those four are worth reading closely, because a plausible-looking example would have got both
wrong:

- **A zero-match `update` and a zero-match `delete` are not errors.** The first returns
  `updated: 0` and the second `matched: 0, deleted: 0`; only `upsert`, which inserts on no match by
  design, can fail for want of a row. So the batch cannot treat "matched nothing" as a failure, or it
  would contradict §8 and the single operations. The no-match message that does exist is `upsert`'s
  required-field one, and it is quoted above because that is the only way a write in this batch can
  fail for matching no rows.
- **The delete-limit message names a remedy a batch does not allow.** The single op's text ends "pass
  `confirm: true` to proceed or `dry_run: true` to preview", and §2 refuses `dry_run` inside a batch. So
  a batch must not forward that message unchanged: it would point a caller at a field the batch rejects.
  The rule this fixes: **where a single op's message names a remedy the batch does not offer, the
  batch's message names the remedy it does.** `confirm: true` is carried through unchanged and
  `dry_run: true` is dropped from the text.

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
`update`, `delete` and `upsert` matches; embed every vectorized write's text through the provider. This is
where a slow or failing provider is absorbed — the writer is not held across a provider round trip, which
is the same property #501 and #509 established for the single operations.

**One write kind cannot be counted outside the writer, and the note has to say so rather than pretend
otherwise.** `update` and `upsert` do pre-count: `updateOrUpsert` has its `preMatched` in hand before it
calls `beginWrite` (`internal/store/update.go:169-194`), and embeds on the same side of that line. A
committing `delete` does not — it takes the writer at `internal/store/search.go:413` and counts inside
it; the pre-writer count in that function belongs to the `dry_run` path, which runs on a read-only
caller transaction. So for a `delete` write the row budget can only be charged *after* the writer is
held, and the enforcement point is inside the transaction: a delete that would push the batch past
1,000 rows is rolled back whole, reported as `invalid_request` naming its index and the cap, like any
other write failure. The delete match cap with `confirm` is checked in the same place, for the same
reason.

That is a real asymmetry between the kinds, and the honest way to present it is as a cost rather than to
paper over it: **the row budget is pre-charged for four kinds and post-charged for `delete`.** A batch
whose only large write is a `delete` therefore holds the writer for the duration of the count, which is
the one case where §9's "1,000 rows touched" is enforced after the fact instead of before it.

Phase 2, inside the transaction: **re-check each table's version, embedding space, dimension and drop
generation**, and re-check the counts the plan was built on. If any moved, roll back and retry the whole
batch, bounded at three attempts (matching the bound `insert` and `updateOrUpsert` already use —
`internal/store/insert.go:70-78` gives up after three).

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

The batch result is an object with a top-level `replayed` boolean (§4) and a `results` array carrying one
entry per write, in order, positionally aligned with `writes` so a caller can zip the two without
matching on anything:

- `insert` → `ids`, `inserted`
- `update` → `updated`
- `delete` → `matched`, `deleted`
- `upsert`, `upsert_by_key` → `inserted`, `updated`, `ids`

This is the union of what the single operations already return, so nothing here is a new result shape —
only a place to put them. The output schema is derived the same way the input is, per kind.

Two details that are deliberate rather than incidental:

- **`delete` returns `matched` as well as `deleted`, on every batch, not only a dry run.** The single op
  requires both fields on every call (`internal/api/ops.go:1642`) and the facade's `DeleteResult.Matched`
  is an unconditional field, so `matched` is not a dry-run-only number that a committing batch could
  drop. In a batch it is also load-bearing rather than decorative: it is the count the §2 row budget is
  charged against (§6 — post-hoc for `delete`, which cannot be counted before the writer), so a caller
  can see what a write matched as well as what it changed.
- **A per-write `replayed` is omitted; the batch's is the one that exists.** `insert` reports `replayed`
  per call, but inside a batch every write is either part of a fresh commit or part of a replayed one, so
  a per-write flag could only ever be the batch's flag repeated. One top-level `replayed` says it once.
  This is the one place the per-write result is not a pure union, and it is a consequence of §4's single
  key rather than a new result concept.

---

## 9. Writer hold time on SQLite

**The batch holds the one writer for all its writes.** That is the cost, and it is why the limits in §2
are row-based rather than statement-based.

What bounds it:

- **1,000 rows touched**, which is the work an `insert` is already allowed to do in one transaction.
  Bounded *before* the write for four kinds; for `delete` it is bounded only once the count has been
  taken under the writer (§6), so a large `delete` is discovered late and undone rather than refused
  early. Late and undone is the weaker guarantee and the note should not pretend otherwise.
- **100 writes**, which bounds the per-statement overhead — schema lookups, temp tables for the
  filter-matched writes, change-log records — that a write costs even when it touches no rows.
- **The operation timeout** (`-op-timeout`, 2 minutes by default, `api.DefaultOpTimeout`), after which
  the transaction is rolled back and the batch is reported as `timeout` like any other operation. `batch`
  gets the plain operation limit, not the `wait_for` extension, since `opLimit` special-cases only
  `migrate` and `wait_for`. Note this is a *server* limit: a batch that trips it is rolled back whole, so
  a caller cannot make it not-trip by trying again, only by sending less.

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

One asymmetry worth naming, because it is a fact about the facade as it stands rather than a choice:
`write.go` has `Insert`, `Update`, `Delete` and `UpsertByKey`, and **no filter-matched `Upsert`**. So
`Store.Batch` would be an embedder's only route to that fifth write kind — a capability the batch
reaches that no single facade method does. That is an argument *for* shipping the batch in the library
rather than an argument against it, and it is a second, independent one from the parity argument above.

---

## 11. Deliberately not in scope

- **`dry_run`.** Tempting and cheap for `insert` and `delete`, where the plan is a count. It is not
  cheap for `upsert`: deciding insert-versus-update without writing requires the plan to be taken
  inside the transaction, which is a different code path from the one that would ship. Getting that
  subtly wrong is worse than not having the operation, so it is the first candidate for a follow-up
  rather than a detail of this one. §2 strips it from every derived write schema, so the single op's
  `dry_run` does not ride along on `delete` by accident; a batch-level dry run is the shape to revisit,
  because it can report every write's plan without any of them committing.
- **Cross-namespace batches** (§1) and **schema changes inside a batch** (§1).
- **Per-write idempotency keys** (§4), and `dry_run` on a write (§2), both refused by index.
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
