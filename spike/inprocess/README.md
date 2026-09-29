# Spike: the in-process pure-Go SQL engine

The comparison Marc asked for on 2026-09-29, part 1: DoltHub's
[go-mysql-server](https://github.com/dolthub/go-mysql-server) as an in-process
`query` engine for the lakehouse tier, against the DuckDB sidecar in
[internal/duckdblockdown](../../internal/duckdblockdown).

**This is a nested module on purpose.** go-mysql-server is not in dolmen's
`go.mod`, and a "no" here leaves the main dependency graph byte-identical — the
main binary builds to the same 49,092,402 bytes with this directory present.
The results below are what Marc needs for Q5; the code is evidence, not a
proposal.

## Verdict: it does not confine, and the two escapes are not closable

`IsReadOnly` and `IsServerLocked` are real and they work for what they cover. But
**`LOAD_FILE()` reads any file and `SELECT ... INTO OUTFILE` writes any file**,
under both settings, and there is no setting that closes either. There is no
`secure_file_priv` equivalent in the library. Q4 rules out a statement filter in
dolmen, so the remaining fix is the one Marc has already ruled out.

**Cross-namespace reads also work in-process.** A database registered on the
provider is reachable by name, and `SHOW DATABASES` / `information_schema.schemata`
enumerate every one of them, so §0.5.2's existence-hiding rules are violated. The
plan's premise for the sidecar — "the engine has nothing else to reach" — is
true of a process and **not** true of an in-process engine sharing one catalog.

### What is refused

| attack | result |
|---|---|
| `CREATE TABLE`, `INSERT`, `UPDATE`, `DELETE`, `DROP` | refused under `IsReadOnly` |
| `LOAD DATA INFILE` | refused |
| `INTO DUMPFILE` | refused |
| `SET GLOBAL secure_file_priv` | refused — the variable is read-only, and exists only to be refused |

### What is not

| attack | result |
|---|---|
| `LOAD_FILE('/etc/hosts')` | **returned the file's bytes** |
| `SELECT body FROM notes INTO OUTFILE '/tmp/x'` | **wrote the rows to disk** |
| `SELECT v FROM otherdb.secrets` | **returned the other namespace's row** |
| `SHOW DATABASES` | **enumerated every registered namespace** |
| `SET GLOBAL local_infile = 1` | accepted |

## Cost

- **Pure Go, verified.** `CGO_ENABLED=0 go build` succeeds, no `runtime/cgo` in
  the dependency graph, and no package in the graph has any `CgoFiles`. This is
  the one thing it does better than the sidecar, which needs a second binary.
- **+60.7 MiB, +130%.** An empty Go main is 2,413,202 bytes; the same main
  importing go-mysql-server is 66,074,418. dolmen is 49,092,402 today, so
  linking it in-process would take the binary to ~107.5 MiB. DuckDB is +19 MB
  and external. A subpackage boundary would keep it off programs that do not use
  the engine, exactly as `postgres.With` does — but the cost lands on every
  program that *does*, and there is no way to avoid it, since "in-process" is the
  whole proposition.
- **One engine per process.** go-mysql-server registers global functions and
  panics (`function 'get_lock' is already registered`) on a second engine, so an
  engine per namespace is not available; the engine is a process-wide singleton
  and namespaces are databases inside it. That is the same in-process
  reachability the confinement table above shows, from the other direction.
- **The dialect is `mysql`, not a neutral SQL.** §D28's obligation is
  disclosure, so this is a legal choice and it is a new value in a set that
  already has `sqlite` and `postgresql`. For callers: backtick quoting, `LIMIT`
  without `OFFSET`, no `ILIKE`, `CONCAT` over `||`, and `LOAD_FILE`/`OUTFILE`
  being *present* is itself a disclosure problem. The README and `skill/dolmen.md`
  would need the dialect named, and the skill's `{{ if eq .Dialect "postgresql" }}`
  branches would need a third.
- **Type mapping is lossy where MySQL is.** A dolmen `boolean` field reads back
  as `int8`, not `bool` (MySQL `BOOLEAN` is `TINYINT(1)`), so the typed-read
  contract in `internal/value` needs a mapping layer. This is the same shape of
  work as adapter #2's NUMERIC normalization, so it is not novel — but it is work.
- **Parquet-backed tables work.** A table over the parquet-go reader, with dolmen
  field types mapped, scans and filters correctly through the engine.

## Rough performance

CI runs the benchmarks; numbers are in the job output. On this laptop, as a
smoke check only: 1,000-row scan ~195 ms/op, filtered ~110 ms/op, 10,000-row scan
~444 ms/op, first query on a cold engine ~40 µs. These are memory-backend
numbers, not Parquet numbers, and a real comparison needs both engines on the
same data — which is the honest reason not to treat them as a verdict on
performance.

## What stays untested

- **Nothing was measured against Parquet at scale** — only three rows.
- **No durability, no concurrency, no catalog.** The memory backend is not a
  store; this spike says nothing about whether a real engine could be built on
  this library.
- **The confinement findings are for v0.20.0.** A later version might add a
  `secure_file_priv`; the tests re-check on every run and log rather than assert,
  so a fix would show up.
- **Not the MySQL wire protocol.** This is the embedded engine, so the MySQL
  port, its authentication and its framing are out of scope and untested.
