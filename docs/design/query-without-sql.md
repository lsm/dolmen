# `query` on an engine with no SQL inside it — decision note

**One decision for Marc. Nothing is planned or built on it.** This is de-risking step 1 of the two the
lakehouse lane needs before anyone plans a lane; step 2, the pure-Go trial, is at the end and is done.

Builds on [storage-adapters-and-auth-research.md](storage-adapters-and-auth-research.md) §2 (which
leaves this open as its own question 2), [storage-adapter-mechanics.md](storage-adapter-mechanics.md)
§1, and [identity-and-engines.md](identity-and-engines.md) §0.5.3 and §9.3.

---

## The question

Adapter #3 (Iceberg/Delta) has no SQL engine inside it — a table format is not a query engine. `query`
is currently contract on every engine, §9.3 permits only `subscribe` to be declared unavailable, and the
shared conformance corpus replays every operation. So either the `query` story is implemented *below the
seam*, or the spec is amended. This note recommends one; the decision is not mine.

Embedded DuckDB is already excluded: it needs cgo, and the binary is `CGO_ENABLED=0` with no cgo
dependency permitted.

## What is already settled, and it narrows the choice more than it looks

**The syntax half is done.** `capabilities` already reports `query_dialect` as an **open enum** —
"named by family (e.g. sqlite, postgresql); an open enum, so branch on it rather than assuming a closed
set". The PostgreSQL adapter landed on that field and declared `postgresql`. A lakehouse engine can
therefore declare whatever dialect it accepts without a new field and without a spec change.

So the amendment in Option B is not about *syntax*. It is only about **availability**, which is
precisely the axis §9.3 currently forecloses. That makes Option B smaller than it first looks — and
also makes it harder to hide: an availability change is the one kind of change every existing caller
can be broken by, and it is the one kind that cannot be rolled out per-engine without a flag day.

**Confinement is the real constraint.** §0.5.3 makes raw-SQL confinement an *engine obligation*: `query`
executes within exactly one namespace and cross-namespace reference must be **impossible by mechanism**.
Note why the existing rule is as detailed as it is — the Postgres adapter has to *reject*
`pg_catalog`/`information_schema` references and function-based probes, because an ordinary role can
read them. That rule exists only because the engine has SQL at all. An option that removes SQL from the
engine does not inherit that rule; it has to earn confinement some other way, and "we validated the
statement" is explicitly not the mechanism the spec accepts.

## Option A — an external SQL engine below the seam

The adapter sits in front of DuckDB as a separate process, Trino, or Spark, and translates dolmen's
validated statement into what that engine accepts.

**The decisive argument is §0.5.3.** An external engine can be handed *exactly* one namespace and
nothing else: DuckDB attached to precisely that namespace's Parquet files, or a catalog scoped to one
namespace. Confinement is then structural — the engine has nothing else to reach, so there is nothing
to reject — which is the strongest form the invariant accepts and strictly stronger than the
mechanism-plus-catalog-rejection the Postgres adapter needs. The expensive, subtle half of §0.5.3 is
already pre-paid by the Postgres work.

**Cost, honestly:**

- **It is a second component.** The dolmen binary stays a single static pure-Go artifact, but the
  *deployment* no longer is one thing. This is research-note open question 3 — whether a non-Go
  sidecar is acceptable operationally given the local-first ethos — and that is a product judgement,
  not a contract one.
- **Every `query` becomes an RPC.** Latency and a failure mode that did not exist: the sidecar being
  down is a new way for `query` to fail, and it needs an error class (a lakehouse tier would want
  `embedder_unavailable`'s shape, which does not exist yet).
- **The dialect translation is real work**, not a passthrough, and it is unbounded work in principle: a
  caller may write any read-only SELECT/WITH the allowlist accepts, and the mapping from that to
  Trino's or DuckDB's SQL is not total. What happens to a statement that translates to nothing is a
  decision, not an implementation detail.

## Option B — amend the spec to permit engines without `query`

§9.3 grows a second availability class beside `subscribe`, `capabilities` reports it, and the
conformance corpus learns that `query` may be absent.

**For it:**

- It is the additive-shaped half of the problem, and it is honest about the truth: a table format
  cannot answer arbitrary SQL, and pretending otherwise with a sidecar is more machinery than the
  capability admits to needing.
- It is cheaper than Option A in every dimension except the one that matters most — §0.5.3.

**Against it, and it is close to decisive:**

- **It breaks a contract callers rely on today.** Per CLAUDE.md the spec is the design authority and
  deviating from it requires editing the spec first in the same PR, so this lands spec-first — correctly,
  and expensively: §0.5.3, §2's op table, `parity_test.go`'s replay of every op, and `capabilities`.
- **Confinement still has to be answered.** An engine with no `query` raises no confinement problem
  (nothing to confine), so the amendment is genuinely small *there*. But the spec would then say
  `query` is optional, and §0.5.3's obligation would need to be reworded to bind only engines that have
  it — a weakening of the invariant that has to be argued on its merits, not slipped in.
- **It moves the cost onto every caller.** An agent that knows `query` works everywhere would have to
  start checking `capabilities` before every use, on an engine tier most callers will never touch. That
  is a permanent tax on the 99% case to serve the 1%.

## Recommendation

**Option A — an external engine below the seam, with DuckDB as a separate process as the first
candidate.**

It is the only option that makes §0.5.3's "impossible by mechanism" true *structurally* rather than by
audit, and it does not weaken a single spec invariant or tax any existing caller. The cost is
operational and visible; Option B's cost is contractual and invisible until a caller is broken by it.
The Postgres adapter is the precedent that "implement it below the seam" is a thing this repo has
actually done rather than merely asserted.

**If the operational cost proves unacceptable — a sidecar on the local tier is a real objection — then
Option B is the fallback, not a co-equal.** And if it is taken it must land spec-first with a *named*
availability class for `query` beside `subscribe`, a reworded §0.5.3 that binds only engines with SQL,
and a conformance corpus that skips `query` by declared capability rather than by engine name.

## The trial: the pure-Go lakehouse tier, tested rather than assumed

De-risking step 2, run since the note above was written. A throwaway program wrote, committed and read
back one table shaped like the conformance tables, and tried a row-level delete both ways.
`CGO_ENABLED=0`, go1.27.0, darwin/arm64, `github.com/apache/iceberg-go v0.6.0` and
`github.com/parquet-go/parquet-go v0.32.0`.

| step | result |
| --- | --- |
| Parquet write then read of a conformance-shaped table — typed columns, a list column, a float32 vector — with `parquet-go` | works |
| Iceberg table creation at format v2 and v3, append 200 rows from Arrow, commit, scan back | works |
| Row-level delete by **position deletes**: a Parquet pos-delete file committed through `RowDelta` | works, 200 → 198 |
| Row-level delete by **deletion vectors**: `dv.DVWriter` then `RowDelta` | commits, and the library's own scanner then fails: `not implemented: deletion vector read is not yet implemented` |
| Catalog: `catalog/sql` over SQLite with the modernc driver | works, no cgo |

**The append path is viable in pure Go today, and the catalog has a local answer.** `catalog/sql`
accepts a `*sql.DB`, and bun's dialects are descriptors rather than drivers — the cgo `sqliteshim` is
not imported — so the modernc driver works and nothing pulls in cgo. For dolmen that means the
namespace's own SQLite file could hold the Iceberg catalog, so commits need no catalog service at all.
That retires the research note's standing worry that a pure-Go Iceberg writer would force a sidecar
process; on this evidence it does not.

**Deletion vectors are write-only in v0.6.0, and that changes the lane's plan.** D25 §0.5.1 and
research §2.2 both answer the small-write problem with "deletion vectors (Iceberg V3/Delta)". In
v0.6.0 a DV write commits successfully and then leaves the table unreadable through the library's own
reader, so a DV-based adapter would be building on a feature that cannot read its own output. The
upstream cause is visible in the source: `table/arrow_scanner.go` notes that its per-file delete maps
change "when readAllDeletionVectors lands", so this is a known gap, not a misconfiguration. The
working answer today is **position deletes**, proven above, or D25's other sanctioned strategy, a WAL
in front of Parquet. A plan that budgets for DVs is budgeting for the wrong one of the two.

**Version pinning is a live hazard, and it lands in this module.** iceberg-go v0.6.0 does not compile
against the current Arrow: `arrow-go/v18 v18.8.0` requires `twmb/avro v1.8.0`, whose `SchemaNode.Root()`
signature change breaks iceberg-go's `internal` package in three places. A clean resolve in a new module
picks the broken combination, so a consumer must hold `arrow-go` at v18.6.0. One pin today; a standing
maintenance cost tomorrow, because a routine dependency bump anywhere in dolmen's graph that wants a
newer Arrow would break the build. That is a cost to weigh against the lane, not a blocker.

**Binary size is real but is not the objection the research note feared.** dolmen's static binary is
49.1 MB. Adding `iceberg-go/table`, `catalog/sql`, Arrow and `parquet-go` takes it to 68.0 MB — **+19 MB,
+39%** — and it stays a `CGO_ENABLED=0` pure-Go static binary with no `runtime/cgo` in the dependency
graph. For scale, Parquet alone is 18.7 MB standalone and the whole Iceberg stack is 70.9 MB. The single
static binary survives; it just gets bigger.

**Three API traps, recorded because each cost the trial more time than the work it guards.**

- **`RowDelta.Commit` does not commit.** It stages the delta onto the transaction; `tx.Commit(ctx)` is
  still required. A `RowDelta.Commit` not followed by `tx.Commit` returns `nil` and leaves the table
  untouched — no snapshot, no error, nothing to notice.
- **A nil `CatalogIO` segfaults.** Building a table with `table.New(..., nil)` and then committing
  panics on a nil dereference inside `doCommit` rather than returning an error.
- **The snapshot summary's `added-data-files` is a count, not a path.** Deletion vectors are keyed by
  data-file path, and the only exported way to enumerate a snapshot's data files is
  `Scan().PlanFiles(ctx)`. Getting it wrong fails the commit with `referenced data files missing`,
  which is at least a clear error, but only once the path is right.

**What this does to the recommendation.** It stands. `query` remains a contract question, and the trial
does not touch it. What the trial does change is the shape of Option A: the *rest* of the lakehouse tier
is now known to be pure Go, so the only cgo component in the whole lane is the SQL engine. That is a
cleaner split than embedding ever was — one sidecar that does only SQL, over a tier that needs none —
and it is the strongest form the argument above can now take.

## What this note does not decide

Which external engine (DuckDB process, Trino, Spark), what a statement that does not translate becomes,
the error class for a sidecar that is down, and whether `query` on a lakehouse tier is worth having at
all given research-note open question 1 — the analytics property or dolmen-side durability. If open
question 1 resolves toward "the analytics property, for Spark/Trino/DuckDB to read the same Parquet",
then a sidecar is what the consumer has anyway and Option A's main objection dissolves.

The lane's schedule is not decided either, and the demand-gate in D25 still stands: nothing above is a
reason to plan adapter #3, only a reason that planning it would no longer be flying blind.
