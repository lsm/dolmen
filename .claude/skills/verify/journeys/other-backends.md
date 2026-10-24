# PostgreSQL and lakehouse engines

For changes under `postgres/`, `lakehouse/`, `internal/postgres`,
`internal/lakehouse`, `sidecar/` or engine-neutral store contracts. These need
services the default setup does not start; if you cannot provide them, report
the step as not verified.

PostgreSQL (CI job `postgres-foundation`): a disposable `postgres:17` with user
`dolmen` / db `dolmen_test`, then the roles from that job's "Create restricted
query and backend roles" step, then:

```bash
export DOLMEN_TEST_PG_DSN='postgres://dolmen_backend:backend-test@127.0.0.1:5432/dolmen_test?sslmode=disable'
export DOLMEN_TEST_PG_REQUIRED=1 DOLMEN_TEST_PG_QUERY_ROLE=dolmen_query
go test -race -count=1 ./internal/postgres
DOLMEN_ENGINE=postgres go test -race -count=1 ./internal/conformance
DOLMEN_ENGINE=postgres go test -race -count=1 ./internal/blackbox
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
