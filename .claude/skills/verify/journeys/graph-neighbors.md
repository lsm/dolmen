# Graph neighbors rungs (#737)

`internal/graph` owns the bounded one-hop algorithm. The SQLite adapter now reads
authoritative relation tables with forward/reverse indexes and request-bound row
scopes. No public neighbors operation is wired yet.

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

Expect six PASS lines: a held read snapshot excludes a concurrent commit while a
new one sees it; owner/table scope filtering; honest candidate truncation; indexed
SEARCH plans in both directions; ordinary update maintenance; and reopen/rebuild
with all owners' authoritative rows retained. Tests also cover denied endpoints,
namespace-read side effects, canceled/failed opens and ambiguous role refusal.
This proves the actual SQLite adapter, not live grants or public neighbors routing.

Run the existing public baselines using SKILL.md:
`go-library.md`, `http-api.md`, `mcp-stdio.md` and `test-suites.md`. Report those as
unchanged-surface baselines. When public wiring lands, add real neighbors calls to
the HTTP/MCP scripts and public example;
only then may the end-to-end #737 acceptance be marked verified.
