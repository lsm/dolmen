# PostgreSQL and lakehouse engines

For changes under `postgres/`, `lakehouse/`, `internal/postgres`,
`internal/lakehouse`, `sidecar/` or engine-neutral store contracts. These need
services the default setup does not start; if you cannot provide them, report
the step as not verified.

PostgreSQL (CI job `postgres-foundation`): a disposable `postgres:17` with user
`dolmen` / db `dolmen_test`, then the roles from that job's "Create restricted
query and backend roles" step. `scripts/pg.sh start` does both in Docker on
`127.0.0.1:55432` (override with `DOLMEN_VERIFY_PG_PORT`) and prints the exports;
`scripts/pg.sh stop` removes the container. On macOS, follow the Colima setup
in SKILL.md without changing the global Docker context, and prefix each `pg.sh`
command below with `DOCKER_CONTEXT=colima`. On other hosts, use the commands as
written with the available Docker context:

```bash
eval "$(.claude/skills/verify/scripts/pg.sh start)"
go test -count=1 -v ./internal/postgres -run 'TestPostgres(ConstraintMigrations|AddConstraintDryRun|ConstraintChangesRunAlone|ForeignKeyDryRun|MigrationKeepsStoredRelation|AcceptReferencesRequiresValue)'
DOLMEN_ENGINE=postgres go test -count=1 -v ./internal/conformance -run '^TestConstraintMigrationBackendIsEnforcedOnHTTPAndMCP$'
go test -race -count=1 ./internal/postgres
DOLMEN_ENGINE=postgres go test -race -count=1 ./internal/conformance
DOLMEN_ENGINE=postgres go test -race -count=1 ./internal/blackbox
.claude/skills/verify/scripts/pg.sh stop
```

Expect existing duplicates and orphans to produce `conflict` during planning and
execution, a feasible dry run to leave schema and reader versions unchanged,
execution to recheck rows after a dry run, and HTTP/MCP writes to enforce the
committed constraint until it is dropped. This slice uses engine migrations to
prepare the schema; wire migration declarations are verified separately when
that API slice lands.

Lakehouse (CI job `lakehouse-foundation`): build the DuckDB sidecar bundle with
`sidecar/duckdb/package.sh <platform> ci <out>`, unpack it, set
`DOLMEN_TEST_DUCKDB_SIDECAR`, `DOLMEN_TEST_DUCKDB_EXTENSIONS` and
`DOLMEN_TEST_DUCKDB_REQUIRED=1`, then:

```bash
go test -race -count=1 ./internal/lakehouse
go test -race -count=1 ./internal/conformance -run '^TestLakehouse'
DOLMEN_ENGINE=lakehouse go test -count=1 ./internal/blackbox
```

With that bundle, run the public Go-library constraint journey:

```bash
go run .claude/skills/verify/scripts/lakehouse-constraints.go "$DOLMEN_TEST_DUCKDB_SIDECAR" "$DOLMEN_TEST_DUCKDB_EXTENSIONS"
```

Expect six `PASS` lines: two declarations read back, partial-unique and orphan
conflicts, RESTRICT on parent deletion, two rows and two constraints after reopen,
and enforcement after reopen. It creates and removes its own synthetic data dir.
The backend tests also cover migration dry-run conflicts/purity and revalidation;
`TestConstraintMigrationBackendIsEnforcedOnHTTPAndMCP` covers both wire write
transports after an engine migration.
