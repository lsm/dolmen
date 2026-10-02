# The DataFusion sidecar (spike)

A minimal sidecar: it registers one namespace's Iceberg table at a pinned snapshot and runs caller
SQL, reading the protocol in [../driver/PROTOCOL.md](../driver/PROTOCOL.md). It exists to be measured
and attacked, not to be the final shape.

## Pinning

`datafusion` is pinned to **exactly** the version `iceberg-datafusion` depends on, which at
`iceberg-datafusion` 0.10.1 is **53.1.0**. That is not a preference. Picking `datafusion` and
`iceberg` independently lands two incompatible Arrow versions in one binary and a wall of type
errors at the batch boundary, which reads exactly like an upstream incompatibility and is not one.

At the correct pin the graph holds one Arrow and one parquet:

```
$ cargo tree -i arrow-array
arrow-array v58.4.0
├── arrow v58.4.0
│   ├── datafusion v53.1.0
│   ...
```

The `sidecar-datafusion` CI job **asserts** this and fails the build if a future bump splits the
Arrow crates again. An earlier draft of this spike recorded the mismatch as a blocker; it was a
pinning mistake, and the correction is recorded in the plan so it is not repeated.

## Reading a pinned snapshot

`iceberg-datafusion` ships two providers and the difference is the whole point:

- **`IcebergTableProvider`** is catalog-backed and **reloads metadata on every scan**, so a reader
  pinned to a snapshot would silently drift onto a newer one.
- **`IcebergStaticTableProvider`** holds one cached table and never refreshes. `try_new_from_table_snapshot`
  takes the snapshot id and refuses an id the table does not contain. That is what a namespace pinned
  to a snapshot needs, and it is what this sidecar registers.

## Confinement

Statements run through
`SQLOptions::new().with_allow_ddl(false).with_allow_dml(false).with_allow_statements(false)`.

**The honest limitation, and it is a real asymmetry against DuckDB:** `SQLOptions` has no
URL-table or external-file knob. DDL, DML and multi-statement plans are refused *because we asked*;
`CREATE EXTERNAL TABLE`, URL tables and `COPY ... TO` are refused by DataFusion's own defaults, which
nobody in this repo owns. The driver measures this rather than asserting it: each sidecar is started
twice, once with `SIDECAR_UNLOCKED=1`, and a refusal that also happens unlocked is reported as a
default rather than as our mechanism.

## Lifecycle

**DataFusion has no cooperative per-query interrupt.** There is no `duckdb_interrupt` equivalent, so
the sidecar holds the `tokio::task::JoinHandle` for the running query and calls `abort()`. That is
strictly coarser: it cancels the task rather than the scan. It is recorded here rather than papered
over, because `cancel` behaving differently between the two engines is a difference dolmen's
supervisor would have to absorb.

Memory is a `FairSpillPool`, not a bounded greedy pool, because the property under test is that a
query too large for the ceiling **spills** rather than failing. **A tight ceiling is an error, not a
spill, when the table is small:** at `maximum_memory` = 180 MB the first CI run failed `init` with
`Out of Memory Error: failed to allocate data of size 32.0 KiB (4.0 KiB/180 bytes used)` before a
single query ran — the ceiling landed below what merely *opening* the table needed. The lifecycle
test records which of "spilled and was right" / "refused with a memory error" happened rather than
assuming a spill, because on this engine the honest answer at a low ceiling is the second one.
