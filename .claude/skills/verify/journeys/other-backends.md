# PostgreSQL and lakehouse engines

For changes under `postgres/`, `lakehouse/`, `internal/postgres`,
`internal/lakehouse`, `sidecar/` or engine-neutral store contracts. These need
services the default setup does not start; if you cannot provide them, report
the step as not verified.

PostgreSQL (CI job `postgres-foundation`): a disposable `postgres:17` with user
`dolmen` / db `dolmen_test`, then the roles from that job's "Create restricted
query and backend roles" step. `scripts/pg.sh start` does both in Docker on
`127.0.0.1:55432` (override with `DOLMEN_VERIFY_PG_PORT`) and prints the exports;
`scripts/pg.sh stop` removes the container. On macOS without Docker Desktop,
`colima start` first. Then:

```bash
eval "$(.claude/skills/verify/scripts/pg.sh start)"
go test -race -count=1 ./internal/postgres
DOLMEN_ENGINE=postgres go test -race -count=1 ./internal/conformance
DOLMEN_ENGINE=postgres go test -race -count=1 ./internal/blackbox
.claude/skills/verify/scripts/pg.sh stop
```

Lakehouse (CI job `lakehouse-foundation`): build the DuckDB sidecar bundle with
`sidecar/duckdb/package.sh <platform> ci <out>`, unpack it, set
`DOLMEN_TEST_DUCKDB_SIDECAR`, `DOLMEN_TEST_DUCKDB_EXTENSIONS` and
`DOLMEN_TEST_DUCKDB_REQUIRED=1`, then:

```bash
go test -race -count=1 ./internal/lakehouse
go test -race -count=1 ./internal/conformance -run '^TestLakehouse'
DOLMEN_ENGINE=lakehouse go test -count=1 ./internal/blackbox
```
