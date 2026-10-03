# The sidecar driver (spike)

One Go driver and one protocol. Spike 3 used it to measure DuckDB and DataFusion *through the same
code and the same attacks*. DuckDB was chosen and the DataFusion sidecar was removed; its results are
in `docs/design/lakehouse-plan.md` §2.8.

Nested module on purpose: it stays out of dolmen's `go.mod`, so a losing engine can be deleted
without touching the main dependency graph.

## What it does

| package | what it is |
|---|---|
| `fixture/` | writes the table the way dolmen's lakehouse engine would: Parquet data files, Iceberg v2 metadata, and **position delete files** for a row-level delete. It is what the readers are tested against, not what is being tested. |
| `sidecar/` | spawns a sidecar and speaks [PROTOCOL.md](PROTOCOL.md). `SIDECAR_BIN` and `SIDECAR_ENGINE` select the engine, so both CI jobs run byte-identical tests. |
| `*_test.go` | the battery: read-back, confinement, lifecycle, perf. |

## Running it

Locally the sidecar is not run — per the lane's standing rule, measurements and engine tests happen
in CI. Compile only:

```sh
gofmt -l .
go vet ./...
CGO_ENABLED=0 go build ./...
```

In CI, `SIDECAR_BIN` selects the engine and these knobs shape the run:

| variable | effect |
|---|---|
| `SIDECAR_BIN` | path to the sidecar; unset means the suite skips |
| `SIDECAR_ENGINE` | label used in log lines (`duckdb`) |
| `SIDECAR_EXT_DIR` | DuckDB only: the pre-placed `iceberg`/`httpfs` extensions |
| `DOLMEN_SPIKE_PERF=1` | run the 10M-row perf set; `DOLMEN_SPIKE_ROWS` overrides the row count |

## The confinement A/B

Each sidecar starts **twice** — once normally, once with `SIDECAR_UNLOCKED=1` — and the same
24-statement battery runs against both. The comparison is what makes the result mean something:

| outcome | what it means |
|---|---|
| refused locked, **allowed unlocked** | the **mechanism**: ours, and load-bearing |
| refused locked **and** unlocked | an engine **default**: holds today, owned by nobody |
| **allowed locked** | a hole, and it fails the build |

A refusal that only proves "the engine said no" is not evidence of confinement. The control is what
turns it into evidence.

## What the fixture deliberately contains

- **Position delete files**, committed through `RowDelta`, so a reader that ignores deletes fails
  rather than passing on a fixture that has none. The delete file's `file_path` column names the
  **data file**, not the table directory — a position delete that names the directory matches nothing
  and the deletes are silently a no-op, which is exactly the kind of bug this fixture exists to
  catch. (It caught one, in the driver, before it could reach an engine.)
- **Three or more snapshots**, so time travel is testable rather than assumed.
- **A secret file and a second namespace's directory** planted outside the namespace, for the
  confinement battery to try to reach.
