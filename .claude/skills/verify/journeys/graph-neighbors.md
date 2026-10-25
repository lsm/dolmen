# Graph neighbors rungs (#737)

The first rung is `internal/graph`: a bounded one-hop algorithm and index/snapshot
read interface, with no SQLite adapter or public neighbors operation yet.

```bash
go test -race ./internal/graph
```

The controlled-view contract tests check hidden seed/edge/endpoint handling, whole
edge/node limits, elapsed stop, caller cancellation, storage errors, stable
`{table,id}` ordering and directions/types. A green result proves only the core
choices. It is not proof of a SQLite read snapshot, adjacency index maintenance,
auth grants, or Go/HTTP/MCP neighbors; these require the following adapter/wire rungs.

For this build-only rung, run the existing public baselines using SKILL.md:
`go-library.md`, `http-api.md`, `mcp-stdio.md` and `test-suites.md`. Report those as
unchanged-surface baselines. When the SQLite adapter lands, extend this file with
its actual snapshot/index, concurrent-write, reopen and RowScope proof. When public
wiring lands, add real neighbors calls to the HTTP/MCP scripts and public example;
only then may the end-to-end #737 acceptance be marked verified.
