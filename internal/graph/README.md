# Graph read core

Shared graph read core. `internal/store` supplies the SQLite adapter; neighbors,
find_path and subgraph are exposed through Go, HTTP and MCP.

`Index.Snapshot` opens one namespace's consistent, read-your-writes view. It owns
any storage pin/transaction until `Close`. `Visible` must apply that request's
table permission, RowScope and existence checks in the same view to a complete
`{table,id}` reference. It must return the same false for hidden and absent rows.
Never resolve row visibility through a separate database snapshot.

`Adjacent` returns incident edge identities with full source/target references,
ordered by relation-table name then edge ID. Return at most the requested candidate
limit and a `more` flag; applying the same direction may narrow it. A self-loop
appears once. Authoritative table writes remain the source of truth. Adapters must
keep adjacency in the same transaction or catch up/fall back before supplying this
snapshot; a separate graph database is not introduced here.

`Neighbors` is exactly one hop. It checks the seed before reading adjacency and
checks the edge and both endpoints before returning an edge. Nodes contain the
visible seed plus complete returned endpoints, deduplicated by table AND ID.
Results sort nodes and edges by table then ID; node limits never produce an edge
with missing endpoints. Relation types filter candidates without expanding them.

Bounds are explicit in this internal API: 1–1000 nodes, 1–1000 examined candidates,
up to 1000 type filters and a positive elapsed budget no greater than 30 seconds.
The candidate budget counts work even when visibility/type filters discard an edge;
`more` means the bounded view cannot certify a complete result. `truncated` is true
with `stop_reason` of `nodes`, `edges` or `elapsed`; `complete` is the only complete
outcome. Caller cancellation and storage faults remain errors, rather than successful
empty results. Snapshot opening and every read use the work deadline. A later public
adapter supplies defaults; this build rung does not settle wire defaults.

Contract tests use controlled synthetic views. They prove the core's decisions,
not SQLite consistency/indexing or public transport behavior; those proofs belong
to the subsequent adapter and wire rungs.

## SQLite adapter

`internal/store.Store.GraphIndex` supplies the default table-backed implementation.
The access callback returns a request-owned immutable RowScope for each snapshot
schema, with Empty for a denied table and nil for trusted unrestricted access.
It must not change schema metadata or call back into storage to resolve a different
schema snapshot. Public transport admission and grants wiring follow separately.

A first read prepares missing forward/reverse indexes under its work deadline,
then opens a read-only transaction and resolves catalog/row scopes inside that view.
Steady-state reads do not acquire the writer when indexes already exist. Indexes
are derived from relation fields; ordinary table transactions maintain them, and
missing indexes rebuild without copying/deleting authoritative rows. Ambiguous
endpoint foreign-key targets are refused by the new reader rather than guessed;
existing constraint declaration rules are unchanged. Candidate pages retain the
existing read-byte bound, besides the graph's row and elapsed bounds.

## Multi-hop traversal

`Traverse` performs BFS over the existing visibility-checked one-hop reader,
borrowing one snapshot for the entire request and closing the underlying view
once. It never reconnects snapshots per hop. Seeds deduplicate by table/id.
`find_path` uses one source and target, returns a shortest unweighted path in
traversal order, and returns empty arrays for an unestablished path (including a
bounded miss). `subgraph` returns sorted references and complete edges.

Traversal bounds: 1–32 hops, 1–1000 visited nodes, 1–1000 distinct returned edges,
1–1000 examined adjacency candidates (`max_work`), and at most 30 seconds including
snapshot setup. Candidate work counts filtered/hidden/repeated edges. Seed checks
are additionally capped at 1000. A remaining frontier at the hop bound stops
conservatively, without another adjacency probe. Exactly reaching a budget is
not proof that unseen adjacency is empty. Reasons are `hops`, `nodes`, `edges`,
`work`, `elapsed` or `complete`; caller cancellation and backend faults remain
errors. A found shortest path is complete even when unrelated branches remain.
