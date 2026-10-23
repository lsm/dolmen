# Per-namespace write serialization benchmark

Gap 6 ([#706](https://github.com/lsm/dolmen/issues/706)); benchmark only.
No writer, connection-pool, schema or Batch behavior changes.

## Reproduce

From the repository root, with Go 1.26.10 available:

```sh
GOTOOLCHAIN=go1.26.10 GOMAXPROCS=8 CGO_ENABLED=0 go test ./internal/store \
  -run '^$' -bench '^BenchmarkNamespaceWrites$' -benchtime=2s -count=3
```

The benchmark uses real SQLite stores in test-owned temporary directories,
created afresh for each case/calibration. It closes stores and removes that data.
There is no live service, credential lookup, external model or network workload.
Each namespace has one notes table with a 256-byte string and a numeric field;
no full-text/vector indexes or embeddings. Default WAL and `synchronous=FULL`
remain enabled. Namespaces are pre-opened, with no eviction or migration.

Cases cross 1/8 namespaces, 1/4/16 concurrent callers and 1/16/128 rows per
`Store.Insert` call. Here **batch size means rows in one Insert transaction**,
not the multi-operation Batch API. Each caller submits the next request only
after its previous call finishes (closed-loop load, no artificial think time).
A shared sequence distributes calls round-robin across namespaces, keeping total
caller count fixed. Setup and payload construction are outside the timer.

`calls/s` is aggregate completed calls divided by elapsed time; `rows/s` multiplies
that by rows per call. The reported p50/p95/p99 are nearest-rank percentiles of
all measured call durations, from immediately before Insert to its return,
including connection/writer wait, validation and commit. They are not per-row
latencies. Concurrent `ns/op` reflects inverse aggregate throughput, not call
latency. Every call must succeed and return the expected ID count; each
namespace's final stored row count must match its assigned calls. Errors fail
the run; they are not discarded from a successful latency distribution.

## Results

Measured at base `b1f90ea6` on an ARM64 developer workstation: 18 logical CPUs,
48 GiB RAM, macOS, local filesystem, eight Go scheduler CPUs. Go 1.26.10,
CGO disabled. Three repetitions per case, with at least two timed seconds each;
1,443–47,624 calls per measured sample. Values below are medians of each run
metric (not pooled percentiles); brackets show the three-run throughput range.
This was a shared workstation, not an isolated performance host.

| Namespaces | Callers | Rows/call | Calls/s median [min–max] | Rows/s median | p50 ms | p95 ms | p99 ms |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 1 | 1 | 8,871 [8,730–9,157] | 8,871 | 0.105 | 0.135 | 0.319 |
| 1 | 1 | 16 | 3,167 [3,143–3,248] | 50,668 | 0.290 | 0.383 | 0.988 |
| 1 | 1 | 128 | 642 [612–648] | 82,188 | 1.446 | 2.001 | 3.746 |
| 1 | 4 | 1 | 8,713 [8,401–8,878] | 8,713 | 0.431 | 0.808 | 1.030 |
| 1 | 4 | 16 | 2,955 [2,861–3,238] | 47,274 | 1.225 | 2.489 | 3.315 |
| 1 | 4 | 128 | 664 [662–668] | 85,009 | 5.705 | 10.120 | 12.500 |
| 1 | 16 | 1 | 3,463 [3,037–4,863] | 3,463 | 1.852 | 13.630 | 56.060 |
| 1 | 16 | 16 | 2,885 [944–3,063] | 46,157 | 4.827 | 11.570 | 17.330 |
| 1 | 16 | 128 | 610 [600–642] | 78,019 | 23.480 | 53.690 | 74.840 |
| 8 | 1 | 1 | 3,034 [1,555–8,739] | 3,034 | 0.126 | 0.278 | 7.511 |
| 8 | 1 | 16 | 3,004 [1,347–3,253] | 48,057 | 0.304 | 0.440 | 0.870 |
| 8 | 1 | 128 | 655 [651–668] | 83,842 | 1.402 | 1.963 | 3.204 |
| 8 | 4 | 1 | 4,197 [4,169–10,448] | 4,197 | 0.207 | 0.796 | 15.930 |
| 8 | 4 | 16 | 1,927 [786–3,867] | 30,826 | 0.894 | 8.005 | 16.090 |
| 8 | 4 | 128 | 678 [634–692] | 86,797 | 5.656 | 7.877 | 14.100 |
| 8 | 16 | 1 | 7,569 [3,266–19,237] | 7,569 | 0.672 | 5.147 | 27.950 |
| 8 | 16 | 16 | 2,946 [2,281–4,378] | 47,143 | 2.951 | 8.802 | 12.800 |
| 8 | 16 | 128 | 693 [672–695] | 88,676 | 20.570 | 51.500 | 77.600 |

## Interpretation and limits

At 128 rows/call, one namespace stays around 610–664 calls/s as callers rise
from 1 to 16, while median-run p99 grows from 3.746 to 74.840 ms. Extra callers
queue rather than increasing this workload's service rate. Batching itself
matters: at one caller, 128 rows/call reaches 82,188 rows/s versus 8,871 rows/s
for single-row calls, at the cost of longer individual calls.

Eight namespaces at 16 callers reach 88,676 rows/s with 128-row batches, only
about 14% above one namespace, with p99 still 77.600 ms. Separate namespace
writers therefore do not promise linear scaling on this filesystem/workload.
Single-row multi-namespace results have especially broad run ranges; do not
present their median as a stable sharding speedup. The calls exercise complete
Insert operations; this does not isolate writer-lock time from filesystem,
checkpoint, allocation, scheduler or other shared-machine effects.

This demonstrates a queueing/throughput ceiling under synthetic saturated load,
not a production incident or an application latency target. It does not cover
HTTP/MCP, auth, mixed reads/updates, multi-operation Batch, FTS, vectors, another
backend, many long-lived namespaces or a controlled dedicated host. No redesign
is made. Any decision about application batching, limiting in-flight writes or
profiling writer/commit wait before considering a storage change belongs to a
separate human-approved task.
