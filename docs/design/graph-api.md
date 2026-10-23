# Graph support: the graph is an API

**Proposal; implementation requires human approval.** Agents manage organisational knowledge through entities and relations, independently of storage. Keep the existing [identity, engine and transaction contract](identity-and-engines.md#06-consistency-contract). Recommend indexed adjacency in the existing SQLite engine first, bounded Go traversal behind an internal adapter, and equivalent operations over the PostgreSQL engine. Storage selection stays internal; unsupported engines declare the graph capability unavailable.

## Agent operations and ontology

Expose these proposed operations through HTTP, MCP and the Go facade with shared conformance cases:

| Operation | Contract |
| --- | --- |
| `add_entity(type, properties)` | Insert through existing table writes; return a stable entity reference. |
| `relate(type, source, target, properties)` | Insert a directed, typed relation; support existing write idempotency. |
| `neighbors(entity, direction, relation_types)` | Return visible incident relations and endpoints. |
| `find_path(source, target, relation_types, max_hops)` | Return a shortest unweighted path within the requested bounds. |
| `subgraph(seeds, direction, relation_types, max_hops)` | Return deduplicated entities and relations reached from seeds. |
| `search_graph(query, entity_types, top_k, expansion)` | Existing semantic search supplies visible seeds; bounded graph expansion returns connections and seed scores separately. |

Entity types (person, project, document, decision) are ordinary typed tables. Relation types (member_of, authored, depends_on) are edge tables with declared source/target entity types and typed properties. Extend existing `create_table`/`migrate` schema management for these declarations; no separate ontology language or implicit inference. References include namespace/table incarnations and row id, preventing name/id reuse from reconnecting old edges. Validate endpoints and restrict deletion of referenced entities in the same transaction, including writes through ordinary CRUD/batch; explicit edge deletion removes the relationship. Schema evolution must preserve or explicitly validate existing endpoints.

Every traversal stays within one namespace, applies table permissions and RowScope to **each edge and endpoint before expansion**, and uses one consistent read snapshot. Hidden nodes cannot connect visible results. Bound hops, visited nodes, edges and elapsed work; return `truncated` and the stopping reason. A bounded miss is not proof that no path exists. Use stable reference ordering for ties; exhausted traversal budgets never become successful complete results. Semantic seeds retain the existing engine's declared exact/ANN behavior.

## Storage decision and research

**Tables are the source of truth; graph adjacency is rebuildable.** Start with forward/reverse indexes over relation rows and Go BFS; recursive SQL is a correctness fallback. SQLite [documents graph CTEs](https://www.sqlite.org/lang_with.html#queries_against_a_graph); PostgreSQL [supports recursion and cycle tracking](https://www.postgresql.org/docs/18/queries-with.html#QUERIES-WITH-CYCLE). This avoids a second commit boundary and keeps `CGO_ENABLED=0` with the current SQLite driver. Do not expose SQL, Cypher or an engine's query language as the graph API.

Native Go candidates researched on 2026-10-23:

| Candidate | Fit and evidence |
| --- | --- |
| [Cayley](https://github.com/cayleygraph/cayley) | Embeddable quads; backend-dependent durability, Apache-2.0. Default-branch commit 2024-07; last release 2019. Upstream's rough 134M-quad / ~150ms intersection claim is dated 2014 hardware, not an organisation-data comparison. |
| [Dgraph](https://github.com/dgraph-io/dgraph) | Active, Apache-2.0, distributed ACID server; its Dockerfile builds CGO-free, but not a supported embedded library. Operational mismatch. |
| [Badger](https://github.com/dgraph-io/badger) | Active, Apache-2.0, embedded transactional KV. Preferred alternate **adjacency index**, if measured benefits justify cross-store recovery; it supplies no graph API itself. |
| [EliasDB](https://github.com/krotik/eliasdb) | Embedded disk graph and transactions, MPL-2.0; latest release 2022, dependency update 2026. Needs crash/durability evaluation. |
| [Gonum graph](https://pkg.go.dev/gonum.org/v1/gonum/graph) / [dominikbraun/graph](https://github.com/dominikbraun/graph) | BSD-3-Clause / Apache-2.0 algorithm libraries; no default durable store. Gonum remains active; latter's latest commit is 2024. |
| [Cayley fork](https://github.com/aperturerobotics/cayley) / [newer GraphDB](https://github.com/dd0wney/graphdb) | Active Go alternatives, Apache-2.0 / MIT. Fork updates Badger; GraphDB claims WAL persistence but requires Go 1.27, above the current 1.26.10 floor. Its throughput/latency figures lack execution context for comparison; rerun before relying on them. |

Go 1.26.10/CGO-free import builds on darwin/arm64 passed for Cayley+Badger, Badger, EliasDB, Gonum, dominikbraun/graph and the fork; this establishes linking, not crash safety. Published benchmarks use incompatible workloads; detailed primary-source comparisons accompany the proposal PR. [Apache AGE](https://github.com/apache/age) adds Cypher inside PostgreSQL but requires a C extension/server. [SQL/PGQ is a standard](https://www.iso.org/standard/79473.html), not a portable deployed backend: PostgreSQL [reverted its implementation](https://github.com/postgres/postgres/commit/b1f106c80cbeb18d3a0219994d98a51a6eca8ede); [DuckPGQ](https://duckdb.org/community_extensions/extensions/duckpgq) is a DuckDB extension, not a pure-Go embed.

## Transactions, backup and validation

Table writes, adjacency changes, idempotency and change records commit together under the current engine contract. A future separate Badger index must consume a durable transaction log, replay idempotently and track an incarnation-scoped checkpoint; queries catch up or fall back to table traversal, never silently serve stale acknowledged writes. Crash recovery and snapshot reads must be proved before enabling it. Graph-as-source-of-truth would require replacing the full table transaction/authorization/backup contract; reject that duplicate authority.

Back up authoritative rows, schemas and existing engine metadata using the engine's supported procedure; adjacency needs no independent backup. Rebuild from a consistent snapshot, replay intervening committed changes, then atomically publish the matching generation; retain table fallback during rebuild and invalidate dropped incarnations. No independent table/graph snapshots masquerading as one consistent backup.

After approval, first pin contracts/ontology, then implement transactional adjacency and bounded traversal, then semantic expansion; evaluate alternative indexes only after a benchmark. Use synthetic 10K/100K entities and 100K/1M relations, with hubs, cycles and permission partitions: neighbors, 1–4-hop expansion/path, semantic seeds plus expansion, 95:5 reads/writes. Compare indexed Go traversal, SQLite/PostgreSQL CTEs and Badger on the same seeds, machine, durability and cache budget; record p50/p95/p99, RSS, disk, write cost, crash recovery and rebuild time. Test hidden-intermediate-node isolation, CRUD bypasses, concurrent endpoint deletion, restart and restore. Proposed targets: p95 neighbors <50ms and bounded 3-hop expansion <250ms on a specified 4-core/16GB host; these are hypotheses, not measured claims. No implementation or benchmark harness is included here.
