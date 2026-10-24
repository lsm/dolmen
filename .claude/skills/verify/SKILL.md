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

- Go: the version in `go.mod`. No cgo (`CGO_ENABLED=0` for builds).
- macOS: resolve the temp dir before running Go tests, or three root-package
  symlink tests fail spuriously (see CLAUDE.md):
  `export TMPDIR="$(cd "$TMPDIR" && pwd -P)"`
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

```bash
S=.claude/skills/verify/scripts
W=$($S/serve.sh)          # server log: $W/server.log, URL: $(cat $W/url)
$S/call.sh "$W" describe_server '{}'
$S/stop.sh "$W"
```

Always run `stop.sh`, even after a failure. If a server dies, read `$W/server.log`.

## Reporting

Paste each script's PASS/FAIL lines and the exact commands you ran. A step you
could not run (no Docker for PostgreSQL, no DuckDB sidecar) is reported as
not verified, never as a pass.

## Keeping this skill current

When a step stops working or a new surface or flag appears, fix it here in the
same PR. Shared setup stays in this file; one journey per file in `journeys/`.
Add a journey as a new file; there is no index to edit.
