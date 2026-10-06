# Running dolmen on the lakehouse engine

`-engine lakehouse` stores every table as an [Apache Iceberg](https://iceberg.apache.org/) table of
Parquet files under the data directory, so other tools that read Iceberg or Parquet can read the
data directly. Everything else is the same as the other engines: the operations, the error contract,
authentication, and the change feed.

```bash
dolmen -engine lakehouse -data /var/lib/dolmen
```

## Layout

Each namespace is a directory `<data>/<namespace>.lakehouse/`:

- `catalog.db` is a SQLite file. It holds the Iceberg catalog plus dolmen's own state: the commit
  log, the change feed and its cursors, row-id allocation, idempotency records, row counts, and
  sealed `secret` values. A `secret` field's Parquet column holds only a presence marker, so a
  Parquet reader never sees ciphertext.
- `data/` holds each table's Iceberg metadata and Parquet files. Updates and deletes are recorded as
  Iceberg position-delete files and do not rewrite data files.

`catalog.db` is the authority. A namespace cannot be rebuilt from its Parquet files alone, because
the counters, cursors and secrets live only in `catalog.db`.

## The SQL sidecar

`query`, and every `filter` (`update`, `delete`, `upsert`, `upsert_by_key`, and the searches), run
in `dolmen-duckdb`, a separate process built on DuckDB. dolmen starts one per namespace on first use
and stops it when the namespace is closed. The sidecar opens the namespace read-only, can read only
that namespace's `data/` directory, and has external access disabled.

| Flag | Environment variable | Default |
|---|---|---|
| `-duckdb-sidecar` | `DOLMEN_DUCKDB_SIDECAR` | `dolmen-duckdb` beside the `dolmen` binary |
| `-duckdb-extensions` | `DOLMEN_DUCKDB_EXTENSIONS` | `duckdb-extensions` beside the `dolmen` binary |

Each release attaches a `dolmen-duckdb-<version>-<platform>.tar.gz` bundle for `linux-amd64`,
`linux-arm64`, `darwin-arm64` and `windows-amd64`. It holds the sidecar, the pinned DuckDB library
beside it, and the pinned `avro`, `iceberg` and `httpfs` extensions under
`duckdb-extensions/<duckdb version>/<platform>/`, where DuckDB looks for them; the sidecar never
downloads an extension. Unpack its contents
into the directory that holds `dolmen`, and the defaults above find it:

```bash
tar -xzf dolmen-duckdb-v0.6.0-linux-amd64.tar.gz --strip-components=1 -C /usr/local/bin
```

To build it yourself, `sidecar/duckdb/package.sh <platform> <version> <out-dir>` fetches the
pinned library and extensions at pinned SHA-256 digests and produces the same bundle.
`sidecar/duckdb/PROTOCOL.md` describes the process protocol. The release SBOM covers the Go module
graph only, not the DuckDB components in the bundle, and the container image does not carry the
sidecar yet.

Without a runnable sidecar the server still starts and serves inserts, `read_rows`, searches
without a filter, the change feed and table management. `query` and filtered operations are refused
with an error that names the sidecar.

`query` and `filter` are DuckDB SQL, and `capabilities` reports `query_dialect` and
`filter_dialect` as `duckdb`. The served skills describe the differences agents need to know.

## Search

Full-text search is computed by dolmen from the stored rows: the core query grammar (terms, `AND`,
`OR`, binary `NOT`, phrases and `term*` prefixes), English stemming and stop words, ranked with
BM25. Vector search is exact cosine similarity. Both read every row of the table, so they suit
tables up to the size where a full scan per search is acceptable.

## Writes and batches

Writes to one namespace are serialized. Each write appends to the commit log in `catalog.db` and is
then applied to Iceberg before the call returns, so a read always sees the writes before it. If the
process stops between the two, the next open replays the log.

A `batch` first snapshots `catalog.db`, so it costs a copy of that file. When any write fails, or the
process stops before the batch commits, the snapshot is restored and nothing from the batch remains.

## Maintenance and backup

Updates and deletes leave position-delete files, and each write adds a data file, so reads slow
down as a table takes many small writes. `vacuum` compacts every table in the namespace: it
rewrites each table's live rows into one Parquet file, drops the delete files, expires every older
snapshot and removes the files only they referenced, then compacts `catalog.db`. Run it during a
quiet period: it holds the namespace's writer, and it waits for running queries and holds new ones
until it finishes. It does not remove every superseded file: each write's earlier
`vN.metadata.json` stays in the table's `metadata/` directory, and a crash in the middle of a
compaction can leave Parquet files nothing references. Neither is ever read again; both cost disk
space only. `dolmen backup` and `dolmen restore` work only on a SQLite
data directory. To back up a lakehouse deployment, stop the server and copy the whole data
directory.
