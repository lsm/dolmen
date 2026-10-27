# Graph search core (#739)

Read shared SKILL.md and run test-suites.md. Before public transports are wired,
the core's consumer surface is the SQLite store adapter. Drive it independently:

```bash
CGO_ENABLED=0 go run ./.claude/skills/verify/scripts/graph-search-core
go test -race ./internal/store ./internal/graph -run 'TestSearchGraph|TestTraversal'
```

Expect four PASS lines: own-row semantic seed id=1 score=1 with two visible
nodes and one edge; truncated/nodes at max_nodes=1 without a dangling edge;
seed property/relation-type filters excluding connections; and an elapsed stop
while snapshot access waits for the deadline. This avoids assumptions about
sub-millisecond timer scheduling on Windows. The consumer
creates and removes its own synthetic database and never downloads a model.
The tests also pin global table/id ties, empty search, hidden seeds/edges/endpoints,
elapsed stops and seed/expansion snapshot consistency across a committed write.

SearchGraph reuses exact cosine scoring and bounded adjacency traversal within
one SQLite read snapshot. The elapsed budget includes seed scoring. top_k is
1–200 across 1–100 tables; property predicates restrict seed selection and
relation_types restrict expansion. Scores describe seeds, not connections.
HTTP, MCP and the public Go facade are a separate follow-up slice.
