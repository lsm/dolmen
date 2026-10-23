# Graph support: the graph is an API

**Proposal; implementation requires human approval.** The graph is a separate module inside Dolmen. Its graph-index interface supports interchangeable SQLite adjacency (initial default), PostgreSQL, Badger and later backends, selected by deployment configuration. Every backend shares the graph API and conformance tests. Keep the existing [identity, engine and transaction contract](identity-and-engines.md#06-consistency-contract).

## Generic graph model and operations

Use existing table writes; expose graph operations through HTTP, MCP and the Go facade:

| Operation | Contract |
| --- | --- |
| `insert` / `upsert` (existing) | Add rows to any table; these rows are graph nodes. |
| `relate(relation_table, source, target, type, properties)` | Insert a directed edge row; support existing write idempotency. |
| `neighbors(node, direction, relation_types)` | Return visible incident relations and endpoints. |
| `find_path(source, target, relation_types, max_hops)` | Return a shortest unweighted path within the requested bounds. |
| `subgraph(seeds, direction, relation_types, max_hops)` | Return deduplicated nodes and edges reached from seeds. |
| `search_graph(query, tables, top_k, expansion)` | Existing semantic search supplies visible seeds; bounded graph expansion returns connections and seed scores separately. |

Any table row is a node; no kind registration is required. Each edge is a relation-table row with source/target row references, a user-defined type and typed properties, declared through existing `create_table`/`migrate`. References include namespace/table incarnations and row id, preventing old edges reconnecting after name/id reuse. Validate endpoints and restrict referenced-row deletion atomically, including ordinary CRUD/batch; delete edges explicitly. Schema evolution preserves or validates endpoints. No domain vocabulary is built in.

Every traversal stays within one namespace, applies table permissions and RowScope to **each edge and endpoint before expansion**, and uses one consistent read snapshot. Hidden nodes cannot connect visible results. Bound hops, visited nodes, edges and elapsed work; return `truncated` and the stopping reason. A bounded miss is not proof that no path exists. Use stable reference ordering for ties; exhausted traversal budgets never become successful complete results. Semantic seeds retain the existing engine's declared exact/ANN behavior.

## Storage decision and research

**Tables are the source of truth; every index backend is rebuildable from them.** The graph module owns traversal and an index interface for committed updates, adjacency reads, snapshot/checkpoint tracking and rebuild. SQLite adjacency uses forward/reverse indexes over relation rows with Go BFS; recursive SQL is a correctness fallback. Backend selection is independent of the authoritative table engine. SQLite [documents graph CTEs](https://www.sqlite.org/lang_with.html#queries_against_a_graph); PostgreSQL [supports recursion and cycle tracking](https://www.postgresql.org/docs/18/queries-with.html#QUERIES-WITH-CYCLE). The SQLite driver preserves `CGO_ENABLED=0`. Do not expose SQL, Cypher or an engine's query language as the graph API.

`Module` means a subsystem within the existing Go module. PostgreSQL and Badger adapters live in separate opt-in subpackages following the [backend option pattern](postgresql.md#selecting-postgresql). The graph core imports neither; the CLI registers adapters for configuration, while embedded consumers import only chosen adapters.

Native Go candidates researched on 2026-10-23:

| Candidate | Fit and evidence |
| --- | --- |
| [Cayley](https://github.com/cayleygraph/cayley) | Embeddable quads; backend-dependent durability, Apache-2.0. Default-branch commit 2024-07; last release 2019. Upstream's rough 134M-quad / ~150ms intersection claim is dated 2014 hardware, not an organisation-data comparison. |
| [Dgraph](https://github.com/dgraph-io/dgraph) | Active, Apache-2.0, distributed ACID server; its Dockerfile builds CGO-free, but not a supported embedded library. Operational mismatch. |
| [Badger](https://github.com/dgraph-io/badger) | Active, Apache-2.0, embedded transactional KV. Pluggable **adjacency index** backend; cross-store recovery remains required. It supplies no graph API itself. |
| [EliasDB](https://github.com/krotik/eliasdb) | Embedded disk graph and transactions, MPL-2.0; latest release 2022, dependency update 2026. Needs crash/durability evaluation. |
| [Gonum graph](https://pkg.go.dev/gonum.org/v1/gonum/graph) / [dominikbraun/graph](https://github.com/dominikbraun/graph) | BSD-3-Clause / Apache-2.0 algorithm libraries; no default durable store. Gonum remains active; latter's latest commit is 2024. |
| [Cayley fork](https://github.com/aperturerobotics/cayley) / [newer GraphDB](https://github.com/dd0wney/graphdb) | Active Go alternatives, Apache-2.0 / MIT. Fork updates Badger; GraphDB claims WAL persistence but requires Go 1.27, above the current 1.26.10 floor. Its throughput/latency figures lack execution context for comparison; rerun before relying on them. |

Go 1.26.10/CGO-free import builds on darwin/arm64 passed for Cayley+Badger, Badger, EliasDB, Gonum, dominikbraun/graph and the fork; this establishes linking, not crash safety. Published benchmarks use incompatible workloads; detailed primary-source comparisons accompany the proposal PR. [Apache AGE](https://github.com/apache/age) adds Cypher inside PostgreSQL but requires a C extension/server. [SQL/PGQ is a standard](https://www.iso.org/standard/79473.html), not a portable deployed backend: PostgreSQL [reverted its implementation](https://github.com/postgres/postgres/commit/b1f106c80cbeb18d3a0219994d98a51a6eca8ede); [DuckPGQ](https://duckdb.org/community_extensions/extensions/duckpgq) is a DuckDB extension, not a pure-Go embed.

## Transactions, backup and validation

Canonical table writes, idempotency and change/outbox records commit together. An index sharing that transaction may update atomically; otherwise it replays committed changes idempotently with an incarnation-scoped checkpoint. Every backend must supply the query's consistent read view or catch up/fall back to authoritative table traversal, preserving read-your-writes. Prove crash recovery and snapshot coordination; never acknowledge an uncoordinated dual write. Graph-as-source-of-truth would replace the full table transaction/authorization/backup contract; reject that duplicate authority.

Back up authoritative rows, schemas and existing engine metadata using the engine's supported procedure; adjacency needs no independent backup. Rebuild from a consistent snapshot, replay intervening committed changes, then atomically publish the matching generation; retain table fallback during rebuild and invalidate dropped incarnations. No independent table/graph snapshots masquerading as one consistent backup.

After design approval and merge, **benchmark SQLite adjacency plus Go traversal versus Badger in the first implementation PR**. Report results before the human chooses a default; pluggability remains required. Contracts, adapters and semantic expansion follow. Use synthetic 10K/100K nodes and 100K/1M edges, with hubs, cycles and permission partitions: neighbors, 1–4-hop expansion/path, semantic seeds plus expansion, 95:5 reads/writes. Compare indexed Go traversal, SQLite/PostgreSQL CTEs and Badger on the same seeds, machine, durability and cache budget; record p50/p95/p99, RSS, disk, write cost, crash recovery and rebuild time. Test hidden-intermediate-node isolation, CRUD bypasses, concurrent endpoint deletion, restart and restore. Proposed targets: p95 neighbors <50ms and bounded 3-hop expansion <250ms on a specified 4-core/16GB host; these are hypotheses, not measured claims. No implementation or benchmark harness is included here.

## Example use: an organisation knowledge base

Users may model people, projects, documents and decisions in tables with application-chosen edge types. Any ontology over this graph belongs to the application.
