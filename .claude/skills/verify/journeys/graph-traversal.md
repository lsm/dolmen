# Bounded find_path and subgraph (#738)

Read shared `SKILL.md`. All rows and data directories are synthetic; use no real
keys or accounts. Run from the worktree root with the Go version in go.mod:

```bash
S=.claude/skills/verify/scripts
W=$(env -u DOLMEN_ADMIN_KEY $S/serve.sh)
trap '$S/stop.sh "$W"' EXIT
$S/graph-traversal.py "$W" "$W/graph-traversal.json"
(cd examples/basic && CGO_ENABLED=0 go build ./... && go vet ./... && go run .)
```

Expect three PASS lines for complete shortest-path/deduplicated-subgraph results
and hop/node/edge/candidate-work stops on HTTP, MCP-over-HTTP and compiled MCP
stdio. A further three PASS lines prove `elapsed` while a synthetic SQLite writer
holds up initial snapshot/index setup: no unreliable large-data timing estimate.
The stdio child stops in finally. Save the JSON and logs before stop.sh deletes W.
The independent Go consumer prints `find_path and subgraph returned 2 nodes and
1 edge: complete`. These runs prove actual binary responses and public Go calls.

Defaults: 4 hops, 1000 nodes/edges/examined adjacency candidates and 1000ms;
caps: 32 hops, 1000 nodes/edges/work and 30000ms. Candidate work includes
filtered/repeated edges. Zero Go options select defaults; explicit wire zero
budgets refuse. A frontier at the hop bound is conservatively truncated without
probing beyond it, even if it might be a leaf. `find_path` returns ordered path
references when found, otherwise empty arrays; `truncated` distinguishes a
bounded miss from an exhausted search. `subgraph` sorts nodes and edges and
never returns an edge with an omitted endpoint.

Run `journeys/test-suites.md`, plus the focused iteration loop:

```bash
go test -race ./internal/graph ./internal/conformance -run 'TestTraversal|TestGraphTraversal'
```

Tests pin all five truncation reasons on both operations, single-snapshot
ownership, hidden seeds/edges/intermediate nodes, per-hop real grants, stable
shortest ties, directions/types, cycles/self-loops, input validation, cancellation
and backend faults. Other graph backends refuse until an adapter is implemented;
this PR does not claim PostgreSQL or Badger traversal.
