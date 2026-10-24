# SQLite/Postgres snapshot to JSONL import

Use synthetic data only. The source-to-target CLI proof uses SQLite plus a
throwaway Postgres 17 source. It executes `examples/import/{sqlite,postgres}.sql`,
compares mapped rows (large IDs, nulls, escaped text, references and UTC timestamp
truncation), interrupts/restarts the CLI, retries original/new keys, and checks
change counts plus conflicting-chunk rollback. An actual server coercion failure
must name `line 1 field body`, omit the synthetic value, and print no commit.
Postgres precision loss must be
`events=2`, `notes=1`, `total=3`; `.999999` must become `.999`, not the next second.

Shared setup (including Colima without a global context change) is in SKILL.md;
use a unique container name/port.

```bash
S=.claude/skills/verify/scripts
W=$($S/serve.sh)
DOCKER_CONTEXT=colima DOLMEN_VERIFY_PG_NAME=dolmen-source-import DOLMEN_VERIFY_PG_PORT=55439 $S/pg.sh start
$S/source-import.py "$W" dolmen-source-import
$S/http-smoke.sh "$W"
$S/mcp-stdio.sh "$W"
# Save $W/source-import.json, exports/precision reports and command output first.
DOCKER_CONTEXT=colima DOLMEN_VERIFY_PG_NAME=dolmen-source-import $S/pg.sh stop
$S/stop.sh "$W"
```

Always stop both services, including on failure. Expect every script line `PASS`;
missing Docker/Postgres or a failed comparison is not a pass. Also run
`journeys/test-suites.md`. The SQL examples use fresh output directories and never
clean up a failed target or modify the source.
