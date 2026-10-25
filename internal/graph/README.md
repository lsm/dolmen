# Graph read core

Internal build rung for #737. Public Go/HTTP/MCP operations and the SQLite adapter
follow separately; this package does not enable a user-facing operation yet.

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
