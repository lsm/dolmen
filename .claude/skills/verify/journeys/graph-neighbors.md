# Graph neighbors rungs (#737)

`internal/graph` owns the bounded one-hop algorithm. The SQLite adapter now reads
authoritative relation tables with forward/reverse indexes and request-bound row
scopes. Public `Neighbors` and HTTP/MCP `neighbors` use this adapter.

```bash
go test -race ./internal/graph
```

The controlled-view contract tests check hidden seed/edge/endpoint handling, whole
edge/node limits, elapsed stop, caller cancellation, storage errors, stable
`{table,id}` ordering and directions/types. A green result proves only the core
choices. It is not proof of a SQLite read snapshot, adjacency index maintenance,
auth grants, or Go/HTTP/MCP neighbors; these require the following adapter/wire rungs.

For the SQLite adapter, run its real storage consumer (synthetic temporary data,
no mocks, no daemon or external service):

```bash
go test -race ./internal/store -run '^TestGraphSQLite'
CGO_ENABLED=0 go run ./.claude/skills/verify/scripts/graph-sqlite
```

Expect seven PASS lines: a held read snapshot excludes a concurrent commit while a
new one sees it; owner/table scope filtering; honest candidate truncation; indexed
SEARCH plans in both directions; ordinary update maintenance; and reopen/rebuild
with all owners' authoritative rows retained; and a located endpoint diagnostic
without revealing a denied relation table. Tests also cover denied endpoints,
namespace-read side effects, canceled/failed opens and ambiguous role refusal.
This proves the actual SQLite adapter, not live grants or public neighbors routing.

Run the Go example and both compiled-daemon transports using SKILL.md:
`go-library.md`, `http-api.md`, `mcp-stdio.md` and `test-suites.md`.
The example prints `neighbors returned 2 nodes and 1 edge: complete`;
HTTP and MCP stdio scripts declare relations and call neighbors, including a
bounded HTTP result. Defaults are both directions, 1000 nodes, 1000 examined
edges and one second; explicit wire zero budgets are refused, while Go zero
options select defaults. Bounds cannot exceed 1000 nodes/edges or 30 seconds.

For actual grants and owner visibility on a separately spawned compiled daemon:

```bash
S=.claude/skills/verify/scripts
W=$($S/serve.sh)
trap '$S/stop.sh "$W"' EXIT
$S/http-smoke.sh "$W"
$S/mcp-stdio.sh "$W"
$S/graph-neighbors.py "$W" "$W/graph-neighbors.json"
```

The last script uses its own synthetic DB, ephemeral loopback port, generated
bootstrap key and API keys. It checks HTTP and MCP-over-HTTP: hidden endpoint
suppression (a plain-table update grant is not read), stable visible references,
explicit node truncation, and owner-filtered seeds/edges. It stops its child in a
finally block; retain the JSON evidence before stop.sh removes W.
No PostgreSQL/lakehouse graph adapter is advertised as implemented.
