---
name: verify
description: Prove a dolmen change works where a user sees it — build the static binary, run it on a throwaway data dir with synthetic rows, drive the HTTP /v1, MCP and Go-library surfaces, and run the test tiers CI runs. Use before calling any dolmen change done.
---

# Verify a dolmen change

Unit tests passing is not proof. Run the change on the surface its user touches
and read the real response. Pick journeys by listing `journeys/` and reading the
ones your change touches; every change also runs `journeys/test-suites.md`.

## Shared setup

All state is synthetic and lives in a fresh temp dir; nothing touches `./data`,
your real `DOLMEN_*` env, or a downloaded embedding model.

- Go: at least the version in `go.mod`, with automatic toolchain selection for dependencies. No cgo (`CGO_ENABLED=0` for builds).
- Vulnerability scans: use a patched Go toolchain and source-run `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` if the installed scanner was built with an older Go parser. The Go 1.27 standard-library certificate advisory requires 1.27.3 or later.
- macOS: resolve the temp dir before running Go tests, or three root-package
  symlink tests fail spuriously (see CLAUDE.md):
  `export TMPDIR="$(cd "$TMPDIR" && pwd -P)"`
- Disposable PostgreSQL uses `scripts/pg.sh` and Docker. On macOS, start Colima
  with `colima start --activate=false` if needed; set `DOCKER_CONTEXT=colima` per
  Docker/pg.sh command. Never change the global Docker context. Use a unique
  `DOLMEN_VERIFY_PG_NAME` and free `DOLMEN_VERIFY_PG_PORT`, and always stop it.
- Scripts live in `scripts/` next to this file (bash 3.2 compatible; need
  `curl` and `python3` for picking a free port):

| Script | Does |
| --- | --- |
| `scripts/serve.sh [flags]` | builds `./cmd/dolmen` into a new `dolmen-verify.*` temp dir, starts it on a free loopback port with `-data $W/data`, `DOLMEN_EMBED_PROVIDER=none`, auth off; waits for `/healthz`; prints the work dir `W`. Extra args go to `dolmen`. |
| `scripts/call.sh W op 'json'` | `POST /v1/{op}` to that server and prints the envelope. |
| `scripts/stop.sh W` | stops the server and deletes `W`. |
| `scripts/lib.sh` | `scrub_env`, sourced by the two launchers: drops every `DOLMEN_*` (except `DOLMEN_ADMIN_KEY`), `OPENAI_API_KEY`, `REMBED_CACHE` and `HF_TOKEN`, as `internal/blackbox` does. |
| `scripts/http-smoke.sh W` | HTTP journey, PASS/FAIL per step. |
| `scripts/mcp-stdio.sh W` | MCP-over-stdio journey using `$W/dolmen`, PASS/FAIL per step. |
| `scripts/source-import.py W CONTAINER` | Synthetic SQLite/Postgres export-to-CLI migration proof; uses the disposable `pg.sh` container with `DOCKER_CONTEXT=colima`. |
| `scripts/graph-neighbors.py W OUTPUT_JSON` | Compiled-daemon HTTP/MCP neighbors with synthetic API keys, table visibility, owner scopes and budget evidence; owns and stops its separate auth daemon. |
| `scripts/graph-traversal.py W OUTPUT_JSON` | Compiled HTTP, MCP-over-HTTP and MCP stdio find_path/subgraph, each truncation reason, and deterministic blocked-snapshot elapsed proof; owns/stops its stdio child. |
| `scripts/graph-checks.sh OUTPUT_PREFIX` | Runs every test-suites.md tier and a source-run vulnerability scan, preserving a log per check at OUTPUT_PREFIX-*.log. Use a patched GOTOOLCHAIN when needed. |
| `scripts/graph-search-core/` | Standalone SQLite graph search consumer with synthetic own-row seeds, properties, relation types and bounds; run with `CGO_ENABLED=0 go run ./.claude/skills/verify/scripts/graph-search-core`. |
| `scripts/graph-sqlite/` | Standalone Go consumer of the SQLite graph adapter; run with `CGO_ENABLED=0 go run ./.claude/skills/verify/scripts/graph-sqlite`. |

```bash
S=.claude/skills/verify/scripts
W=$($S/serve.sh)          # server log: $W/server.log, URL: $(cat $W/url)
$S/call.sh "$W" describe_server '{}'
$S/stop.sh "$W"
```

Always run `stop.sh`, even after a failure. If a server dies, read `$W/server.log`.
When a command runner reaps background children as its command exits, run
`serve.sh`, the checks, and `stop.sh` in one shell invocation with an EXIT trap.

## Reporting

Paste each script's PASS/FAIL lines and the exact commands you ran. A step you
could not run (no Docker for PostgreSQL, no DuckDB sidecar) is reported as
not verified, never as a pass.

## Keeping this skill current

When a step stops working or a new surface or flag appears, fix it here in the
same PR. Shared setup stays in this file; one journey per file in `journeys/`.
Add a journey as a new file; there is no index to edit.
