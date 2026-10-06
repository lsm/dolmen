# dolmen-duckdb

The lakehouse engine's SQL sidecar: a small C++ program linked against the pinned prebuilt
`libduckdb`, with the `iceberg` and `httpfs` extension files placed in a directory dolmen names.
dolmen starts one per namespace, the first time a `query` reaches that namespace, and keeps it
until the namespace handle is evicted, the namespace is dropped, the table set changes shape, or
the store closes. It is not the DuckDB CLI: the CLI treats a line starting with `.` as a command
and runs `.shell`, which no setting can switch off (lakehouse plan §2.2), while this program reads
only framed messages and never interprets SQL text itself.

## Building

```sh
sidecar/duckdb/fetch-deps.sh lib libduckdb-linux-amd64.zip "$dest"
sidecar/duckdb/fetch-deps.sh ext linux_amd64 "$dest/ext"
g++ -std=c++17 -O2 -I"$dest" sidecar/duckdb/dolmen-duckdb.cpp -L"$dest" -lduckdb -Wl,-rpath,"$dest" -o "$dest/dolmen-duckdb"
```

`fetch-deps.sh` downloads over HTTPS and refuses any file whose sha256 is not the pinned one. The
CI job `lakehouse-foundation` builds it this way on Linux, macOS and Windows and runs the
lakehouse SQL tests against it.

## Environment

dolmen starts the process with a fixed environment and nothing inherited:

| variable | meaning |
|---|---|
| `DOLMEN_DUCKDB_DATA_DIR` | the namespace's data directory, the only directory the sealed engine may read |
| `DOLMEN_DUCKDB_EXTENSION_DIR` | where `iceberg.duckdb_extension` is loaded from; nothing is downloaded |
| `DOLMEN_DUCKDB_MEMORY` | DuckDB's memory ceiling in bytes, `0` for DuckDB's default |
| `DOLMEN_DUCKDB_THREADS` | DuckDB's thread count, `0` for DuckDB's default |
| `HOME`, `USERPROFILE` | a private empty directory dolmen creates and removes |
| `TMPDIR`, `TMP`, `TEMP` | where the private catalog file and its spill directory are made |

## Framing

One message per line on stdin, one reply per line on stdout. Fields are separated by a tab, and
inside a field `\` is written `\\`, a tab `\t`, a newline `\n` and a carriage return `\r`, so SQL
and values containing any of them arrive intact. The first line the process writes is
`0<TAB>ready`. A request starts with an id the reply repeats; id `0` is used for messages that get
no reply.

| request | reply |
|---|---|
| `<id> define <statement>` | `<id> ok`; runs a statement dolmen built, before `seal` only |
| `<id> seal` | `<id> ok`; applies the confinement below, after which `define` is refused |
| `<id> query <offset> <limit> <max_bytes> <sql> <arg>...` | the result below |
| `0 cancel` | none; interrupts the running query, which then replies `canceled` |
| `<id> shutdown` | `<id> ok`, then the process exits |

An error reply is `<id> error <class> <message>`, with `class` one of `query_error` (SQL the engine
rejected), `invalid` (more than one statement), `canceled`, `resource` (out of memory), `too_large`
(the first row alone is over `max_bytes`) or `internal`.

A query's arguments and result cells are tagged values: `n` null, `b0`/`b1` a boolean, `i<n>` an
integer, `f<repr>` a double, `d<text>` an exact number too large for an integer, `s<text>` text and
`x<hex>` bytes. dolmen sends a boolean argument as `i1`/`i0`, binding it as the integer SQLite
would; the sidecar still accepts `b0`/`b1`, and returns booleans in result cells that way. The result
reply is

```
<id> ok <truncated 0|1> <ncols> <name> <type> ... <nrows> <cell> ...
```

with the cells in row order. `truncated` is `1` when rows exist past `limit` or past `max_bytes`.

## Confinement

`seal` reopens the private catalog file `READ_ONLY` with `allowed_directories` set to the data
directory and the spill directory, fixes the memory and thread ceilings in the configuration,
loads `iceberg`, and then sets `enable_external_access = false`,
`autoinstall_known_extensions = false`, `autoload_known_extensions = false`,
`allow_persistent_secrets = false` and finally `lock_configuration = true`. Any refusal stops the
process rather than leaving it running unlocked. The order matters, and the reasons are recorded in
`docs/design/lakehouse-plan.md` §2.4–§2.8.
