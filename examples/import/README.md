# Import a mapped snapshot

These executable SQL recipes map two synthetic application tables: `events`
(`id`, `created_at`, `body`) and `notes` (the same plus `event_id`). IDs and
references are positive integral keys below MaxInt64; `body` is nullable text.
SQLite creation strings are already canonical UTC milliseconds. PostgreSQL uses
`timestamptz`: `date_trunc` truncates, never rounds, to UTC milliseconds, and
`precision.json` counts rows losing precision per table and in total. Preserve an
original instant in a separately mapped text field if that precision matters.

Adapt the explicit mapping and create matching target tables before import. These
are recipes for this schema, not a generic schema converter. No decimal, binary,
file, ambiguous local time, UUID or composite-key conversion is implied. Leave
unsupported values for an explicit mapping; do not cast them silently. Never
export internal tables, identities, grants or secret keys. Native JSON builders
preserve integer tokens, nulls and escaped text; the client uses `UseNumber`.
Quiesce source writes through comparison and cutover. Keep the source and export.

Run from a new export directory (all files are relative to it):

```sh
sqlite3 -readonly -bail source.db < /path/to/examples/import/sqlite.sql
# PGHOST/PGPORT/PGDATABASE/PGUSER and optional PGPASSWORD are environment settings.
psql -X -qAt -f /path/to/examples/import/postgres.sql
```

Use one recipe for each source: each reads both tables and its loss report in one
snapshot (SQLite read transaction; Postgres repeatable-read/read-only transaction).
The scripts overwrite their output files; use a new directory for each immutable
snapshot. SQLite's report is zero because this mapping requires canonical input.
Inspect `precision.json` before accepting the timestamp mapping.

```sh
# Create events: body=text; notes: body=text, event_id=number, using create_table.
# Set DOLMEN_TOKEN in the environment for an authenticated target; never a flag.
dolmen import -url http://127.0.0.1:8790 -namespace demo -table events -snapshot export-a < events.jsonl
dolmen import -url http://127.0.0.1:8790 -namespace demo -table notes -snapshot export-a < notes.jsonl
```

The default chunk caps are 1000 rows and 33554432 encoded request bytes, including
namespace/table/key overhead and JSON escaping. Lower them with `-rows`/`-bytes`.
Each raw input line is bounded at 32 MiB even when its encoded object is smaller.
Keys are `snapshot/table/chunk` (one-based). Restart with the same immutable input,
name and chunk options to replay receipts. Changed grouping or content needs a new
snapshot name; identical existing rows then skip. The first failure stops the
stream, naming the input line/field without printing values or server messages;
only acknowledged commits print. Prior chunks remain committed. Empty input is
an error. Compare counts, ordered IDs, timestamps, values and references via
`read_rows`/`query` before switching readers. Rebuild derived indexes normally.

Repeat the synthetic end-to-end proof with the verify skill's
`journeys/source-import.md`. It exercises the actual CLI, not an HTTP mock.
