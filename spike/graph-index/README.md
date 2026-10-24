# Graph adjacency backend benchmark (#718)

This standalone spike compares SQLite adjacency indexes with Badger using the same Go BFS. It implements the first measurement step in [the graph design](../../docs/design/graph-api.md), not the production graph API. **No default backend is selected.** The command requires `-backend sqlite` or `-backend badger`. Root Dolmen dependencies and runtime behavior are unchanged.

The interface is only atomic append, a consistent adjacency read snapshot, and close. SQLite stores rows with `(src,dst,id)` / `(dst,src,id)` indexes; Badger stores canonical edge IDs and big-endian forward/reverse adjacency keys. Both reject duplicate IDs atomically. There is no outbox, checkpoint, snapshot publication, table integration or extra backend. Production permission/CRUD/incarnation/restore conformance remains future work. Here, fixed node/edge admission callbacks model permission partitions and hidden bridges.

## Repeatable verification

From this module:

```sh
GOTOOLCHAIN=go1.26.10 go test -race ./...
GOTOOLCHAIN=go1.26.10 CGO_ENABLED=0 go build ./...
./run.sh /absolute/path/to/new-owned-evidence-directory
```

The runner builds a CGO-free binary, then runs separate processes for SQLite and Badger at 10,000 nodes/100,000 edges and 100,000 nodes/1,000,000 edges. It leaves all synthetic databases and JSON evidence in the requested directory. It never opens the Dolmen daemon or user data. Run sequentially on a quiet host; do not run tests/builds alongside timing. `compare.py` refuses a comparison unless both adapters have identical result checksums, scanned-edge counts, found counts and truncation counts in every case. Contract tests cover snapshot isolation with concurrent committed writes, batch rollback, stable shortest paths, node/edge/time caps, hidden intermediates, cancellation, clean reopen, and an acknowledged append followed by abrupt process exit without Close. The contracts were committed and failed before the adapters/traversal existed.

## Measurement contract

- Go 1.26.10, `GOMAXPROCS=4`; machine/OS recorded in `host.txt`. This is not an isolated 4-core/16 GiB server, so the design's proposed targets are not certified here.
- Fixed PCG seed `(718,1)`, one ring cycle through all nodes, remaining random directed edges, about 5% hub edges from node 1. Every fourth 100-node block is hidden; every 17th edge is hidden. Three precomputed visible seeds model semantic expansion; embedding/search cost is excluded.
- Both use synchronous durable writes: SQLite WAL + `synchronous=FULL`, Badger `SyncWrites=true`. Initial load/rebuild use identical 1,000-edge atomic batches. SQLite has two connections with 32 MiB page cache each, mmap disabled; Badger has 48 MiB block + 16 MiB index cache, two 16 MiB memtables and 64 MiB value-log files. Equal configured read-cache ceilings do not imply equal actual memory; native/database buffers and Go overhead differ.
- Twenty two-hop warmups, then 100 samples per read case. Timings include opening/pinning a snapshot, traversal, result sorting, and closing the snapshot. Neighbor queries are one-hop expansion; hub neighbors are separate. Expansion/path use 1–4 hops, at most 4,096 admitted nodes, 20,000 processed raw edges, and two seconds. Hidden edges consume the raw-edge budget before admission. Paths use BFS with stable neighbor/edge-ID ordering. A path found at a budget boundary still succeeds; a bounded miss is explicitly truncated. Expansion is complete only within its requested hop radius, with node/edge/time truncation reported separately.
- Mixed workload is 200 sequential operations: 190 three-hop reads and 10 single-edge durable appends, interleaved 19:1. Read and write latency/throughput are reported separately, with their sample counts. This is a sequential 95:5 workload, not a concurrent contention benchmark; ten write samples cannot establish a robust tail latency.
- Ops/s is samples divided by their summed operation durations, not an end-to-end mixed-workload rate. Percentiles use nearest rank; p99 from 100 read samples is descriptive, not a statistical tail guarantee. Counts/checksums expose bounded or short-circuited work instead of comparing latency alone.
- Disk is measured with the main index open after mixed work: apparent file bytes and allocated blocks (WAL/SHM, LSM/value logs included). Preallocation can make apparent size much larger. Peak RSS is the process high-water mark through timed workloads; heap and Go Sys are sampled after GC while the index/fixture remain live. RSS excludes the later rebuild and recovery child.
- Load includes open/schema creation and durable fixture batches. Clean reopen includes open plus a three-hop read. Rebuild creates a fresh index from the same fixture and includes close; it measures offline index construction only. Abrupt recovery loads the fixture in a child and exits immediately after acknowledged durable commits without Close; recovery timing includes open plus a one-hop read, compared with the rebuilt fixture. This is a process-exit recovery probe, not power-loss testing or cross-store coordinated recovery.

The only shared-file change is one contract-test step in the existing CI test job. Measurements and limitations are recorded in RESULTS.md after running the real executable. The human chooses the default after reviewing those results.
