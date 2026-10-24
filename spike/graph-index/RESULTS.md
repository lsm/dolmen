# Measured results: SQLite adjacency vs Badger

Measured on 2026-10-23, Apple M5 Pro / darwin arm64, 48 GiB RAM, 18 logical CPUs, Go 1.26.10, `GOMAXPROCS=4`, CGO disabled. The laptop was not an isolated benchmark host. Each backend/scale ran in a separate process, sequentially; OS caches were not purged. Runtime/adapters/runner were built from `5ee8c3420749c6756208acb8e7f849b30d93ff63`; the comparison script was subsequently made compatible with Python 3.9 (`zip(strict=...)` removed), without changing measured code or data.

The four [JSON reports](results/) retain every sample summary and work checksum; only their absolute evidence-directory paths are normalized. [README](README.md) records the settings and reproduction commands. All 13 cases at each size matched result checksums, scans, found paths and truncation counts across backends. Both abrupt-exit readback probes and snapshot/rollback contracts passed.

**Recommendation for the human's decision:** use SQLite for the initial embedded default if the priority is lower process memory, simple integration with Dolmen's existing SQLite authority, and quick reopen; retain Badger as the read-heavy option. At 100K nodes SQLite used 160 MiB peak RSS versus 557 MiB, and abrupt-exit reopen/read took 28 ms versus 109 ms. Badger delivered about 2x the three/four-hop throughput and built the million-edge index about 7.6x faster. Those are meaningful reasons to choose Badger when rebuild speed/read throughput outweigh memory. **This PR selects no default; the human decides.** Production transaction/outbox integration is not measured here, so this recommendation remains provisional.

Caveats: 100 samples cannot certify p99; the mixed case has only ten writes. These are warm, sequential reads, not concurrent stress or a cold/power-loss recovery test. Every 100K hub-neighbor sample and nine 100K four-hop expansion samples reached a work cap. Hub results are bounded partial results, not full adjacency enumeration; latency targets cannot be claimed for complete hub answers. Expansion is within the requested hop radius. Mixed read/write ops/s are per-operation-class rates, not whole-workload throughput. Apparent Badger disk includes preallocated files; allocated bytes are also reported. Peak RSS excludes the later rebuild/recovery child. Recovery validates an acknowledged probe against a rebuilt fixture, not every record after hardware failure.


10,000 nodes / 100,000 edges: identical result checksums, scans and truncation
| Backend | Case | ops/s | p50 ms | p95 ms | p99 ms | truncated/samples |
|---|---|---:|---:|---:|---:|---:|
| sqlite | neighbors | 16638.3 | 0.024 | 0.043 | 0.150 | 0/100 |
| sqlite | hub_neighbors | 310.2 | 3.192 | 3.420 | 3.480 | 0/100 |
| sqlite | expand_1 | 17864.9 | 0.024 | 0.032 | 0.044 | 0/100 |
| sqlite | path_1 | 19446.9 | 0.022 | 0.028 | 0.035 | 0/100 |
| sqlite | expand_2 | 4025.6 | 0.141 | 0.231 | 0.271 | 1/100 |
| sqlite | path_2 | 7628.4 | 0.092 | 0.196 | 0.329 | 0/100 |
| sqlite | expand_3 | 910.6 | 0.948 | 1.728 | 1.892 | 1/100 |
| sqlite | path_3 | 1573.4 | 0.546 | 1.325 | 1.723 | 0/100 |
| sqlite | expand_4 | 140.7 | 6.699 | 11.079 | 12.466 | 2/100 |
| sqlite | path_4 | 262.3 | 3.256 | 8.157 | 9.122 | 0/100 |
| sqlite | semantic_seeds_expand_3 | 353.2 | 2.747 | 4.220 | 5.228 | 1/100 |
| sqlite | mixed_95_reads | 857.2 | 0.985 | 1.678 | 10.411 | 3/190 |
| sqlite | mixed_5_writes | 8348.4 | 0.102 | 0.207 | 0.207 | 0/10 |
| badger | neighbors | 34130.7 | 0.011 | 0.018 | 0.026 | 0/100 |
| badger | hub_neighbors | 559.6 | 1.783 | 1.854 | 2.048 | 0/100 |
| badger | expand_1 | 36629.0 | 0.010 | 0.017 | 0.021 | 0/100 |
| badger | path_1 | 48565.2 | 0.008 | 0.012 | 0.017 | 0/100 |
| badger | expand_2 | 7773.8 | 0.079 | 0.125 | 0.146 | 1/100 |
| badger | path_2 | 16790.3 | 0.041 | 0.092 | 0.132 | 0/100 |
| badger | expand_3 | 1872.8 | 0.466 | 0.789 | 0.965 | 1/100 |
| badger | path_3 | 3385.6 | 0.235 | 0.633 | 0.804 | 0/100 |
| badger | expand_4 | 307.2 | 3.120 | 5.080 | 5.968 | 2/100 |
| badger | path_4 | 552.7 | 1.535 | 4.008 | 4.692 | 0/100 |
| badger | semantic_seeds_expand_3 | 615.2 | 1.564 | 2.420 | 3.378 | 1/100 |
| badger | mixed_95_reads | 1741.1 | 0.484 | 0.913 | 5.089 | 3/190 |
| badger | mixed_5_writes | 7885.7 | 0.119 | 0.202 | 0.202 | 0/10 |
| Backend | peak RSS MiB | heap MiB | disk apparent/allocated MiB | load ms | reopen ms | rebuild ms | abrupt reopen ms |
|---|---:|---:|---:|---:|---:|---:|---:|
| sqlite | 43.1 | 5.1 | 11.8/11.8 | 488.5 | 11.7 | 433.8 | 6.0 |
| badger | 172.8 | 57.7 | 166.2/7.1 | 249.3 | 12.7 | 281.2 | 200.8 |

100,000 nodes / 1,000,000 edges: identical result checksums, scans and truncation
| Backend | Case | ops/s | p50 ms | p95 ms | p99 ms | truncated/samples |
|---|---|---:|---:|---:|---:|---:|
| sqlite | neighbors | 4666.3 | 0.029 | 0.039 | 0.083 | 1/100 |
| sqlite | hub_neighbors | 45.7 | 21.441 | 27.543 | 28.467 | 100/100 |
| sqlite | expand_1 | 4847.4 | 0.027 | 0.034 | 0.086 | 1/100 |
| sqlite | path_1 | 5124.1 | 0.026 | 0.035 | 0.089 | 0/100 |
| sqlite | expand_2 | 2294.0 | 0.211 | 0.567 | 0.755 | 1/100 |
| sqlite | path_2 | 3272.3 | 0.094 | 0.217 | 0.243 | 1/100 |
| sqlite | expand_3 | 645.9 | 1.329 | 2.328 | 2.839 | 1/100 |
| sqlite | path_3 | 1116.1 | 0.619 | 1.628 | 2.085 | 0/100 |
| sqlite | expand_4 | 100.2 | 9.693 | 16.127 | 19.012 | 9/100 |
| sqlite | path_4 | 183.9 | 4.585 | 13.111 | 14.649 | 2/100 |
| sqlite | semantic_seeds_expand_3 | 245.3 | 3.677 | 5.925 | 8.838 | 1/100 |
| sqlite | mixed_95_reads | 708.4 | 1.289 | 2.108 | 3.027 | 1/190 |
| sqlite | mixed_5_writes | 7290.2 | 0.134 | 0.172 | 0.172 | 0/10 |
| badger | neighbors | 11703.5 | 0.015 | 0.023 | 0.031 | 1/100 |
| badger | hub_neighbors | 143.9 | 6.878 | 7.298 | 7.372 | 100/100 |
| badger | expand_1 | 12231.6 | 0.014 | 0.020 | 0.022 | 1/100 |
| badger | path_1 | 14138.1 | 0.010 | 0.015 | 0.017 | 0/100 |
| badger | expand_2 | 5719.1 | 0.108 | 0.163 | 0.179 | 1/100 |
| badger | path_2 | 8432.6 | 0.044 | 0.102 | 0.112 | 1/100 |
| badger | expand_3 | 1282.3 | 0.716 | 1.229 | 1.350 | 1/100 |
| badger | path_3 | 2440.4 | 0.291 | 0.766 | 1.038 | 0/100 |
| badger | expand_4 | 202.6 | 4.722 | 8.127 | 9.719 | 9/100 |
| badger | path_4 | 387.7 | 2.168 | 6.528 | 7.007 | 2/100 |
| badger | semantic_seeds_expand_3 | 544.5 | 1.697 | 2.703 | 3.888 | 1/100 |
| badger | mixed_95_reads | 1409.5 | 0.631 | 1.278 | 1.495 | 1/190 |
| badger | mixed_5_writes | 6154.0 | 0.163 | 0.188 | 0.188 | 0/10 |
| Backend | peak RSS MiB | heap MiB | disk apparent/allocated MiB | load ms | reopen ms | rebuild ms | abrupt reopen ms |
|---|---:|---:|---:|---:|---:|---:|---:|
| sqlite | 159.9 | 32.6 | 65.1/66.3 | 21451.6 | 25.6 | 20724.0 | 27.9 |
| badger | 557.4 | 263.0 | 221.9/69.0 | 2823.2 | 13.4 | 3055.0 | 108.9 |
