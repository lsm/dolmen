# Embedding backfill

Status: the design for [#53](https://github.com/lsm/dolmen/issues/53) (resumable vectorizing
migrations), decided before implementation. Each numbered section names the tests that hold it.
This file introduces no behavior of its own; where an item has no repo pin yet it says so.

## What was wrong

On SQLite, `Store.Migrate` opened the namespace's single write transaction
(`internal/store/migrate.go`) and called the embedding provider inside it. One large
`set_vectorize` therefore held the only writer for the whole backfill, so every other write to
that namespace blocked until it finished or hit its deadline. Each batch also re-scanned
`_embedding IS NULL` from the start of the table, so a table with already-embedded rows paid for
them on every pass.

PostgreSQL already embeds outside the write transaction — `backfillEmbeddings` reads keyset pages
and `applyEmbeddings` runs in the write transaction, pinned by
`TestPostgresMigrateEmbedsOutsideWriteTransaction` — but it keeps every vector in memory and
re-embeds every row on each of its three retries.

Both engines threw away all embedding work when a migrate was interrupted: the client
disconnected, `-migrate-timeout` fired, the provider failed, or the process crashed. The next
attempt started again from the first row.

## 1. The stage lives on disk, keyed by identity and content

A staged vector is stored outside the table. A row's key is

| part | why it is in the key |
| --- | --- |
| table drop generation | a `drop_table` and a fresh `create_table` of the same name must not share a stage |
| row id | the row being filled |
| provider identity | a stage made by one model is meaningless to another |
| SHA-256 of the source text | a row whose text changed has a different vector |

**A staged vector counts for a row only while the row's current text has that digest.** That single
invariant is what makes every other rule fall out, and it is what lets a binary that knows nothing
about stages read a file that has them (see *Catalog format*).

SQLite keeps the stage in the namespace file, in a `_dolmen_embed_stage` catalog table.
PostgreSQL keeps it in new catalog relations. Neither is a user table and neither is visible to
`query`: the read-only SQL allowlist is unchanged.

## 2. Embedding happens outside every write transaction

`Migrate` becomes three phases, and only the last one takes the writer.

1. **Validate on the read side.** The whole plan is computed against a read-only transaction
   first, including `expected_version` and `expected_incarnation`, so a plan that cannot apply is
   refused before any provider call.
2. **Backfill on the read side.** The table is walked in keyset pages (`id > last ORDER BY id
   LIMIT 128`, the page size PostgreSQL already uses). Rows whose digest is already staged are
   skipped; the rest are embedded. Each finished page is written to the stage in a short write
   transaction. No write transaction is open while the provider runs.
3. **Activate in one short write transaction.** See item 4.

The read that decides what to embed and the write that records the result are separate, so the
cursor can only move forward past rows that are genuinely staged.

## 3. The text is read from where it lives

A field that is being vectorized may not exist yet, and the column it will live in may be renamed
by the same migration. The planner resolves the source before any embedding:

- a field already on disk reads its own column — the *old* physical name if the migration renames
  it, so the text is the text the row has today;
- an `add_field` with a string default has no column to read, and takes a single provider call
  whose result applies to every row.

PostgreSQL's planner already carries this as `embed.source` and `embed.constant`
(`internal/postgres/migrate.go`); SQLite's `planMigration` decides between the two the same way
when it counts `EmbedRows`.

## 4. Activation is one short write transaction

Inside the single write transaction, and only after a full coverage check has already passed on
the read side:

1. re-plan against the current schema, so a concurrent migration that landed first is seen;
2. confirm every row with non-empty text has a staged vector whose digest matches that row's
   current text;
3. apply the steps and fill `_embedding` from the stage;
4. set the embedding space and dimension, bump the version, record the migration;
5. delete the table's stage.

The coverage check runs on the read side first because it can be expensive and must not hold the
writer. Inside the transaction, only the rows the change log shows as written since that check
are re-checked — the same walk `vecCache.catchUp` performs. If rows were written in the
meantime, the migrate embeds them outside the writer and tries activation again, up to a limit.

Past the limit it returns a conflict that says re-issuing the same `migrate` continues from where
it stopped. It does not return a timeout and it does not lose the stage.

## 5. Resuming is re-issuing

There is no job table and no new operation. Running the same `migrate` again embeds only the rows
that are not yet staged, because the stage is keyed by content and the walk skips what matches.
The table stays at its old version until activation, which `TestAKilledMigrateLeavesTheTableConsistent`
already pins.

## 6. Stale stages and overlapping migrates

- A stage is never used across a different provider identity, or across a table that was dropped
  and recreated. Both are excluded by the key in item 1, and are asserted by
  `TestAStagedVectorIsNotUsedAfterTheProviderChanges` and
  `TestAStagedVectorIsNotUsedAfterTheTableIsRecreated`.
- A table's stage is deleted on activation and on `drop_table`. `vacuum` deletes it too, when no
  `migrate` of that table is running in this process.
- Two migrates of one table may overlap, and on PostgreSQL they may be in different processes.
  They stay correct because the stage is keyed by content and activation re-checks coverage inside
  its own transaction; duplicate provider calls are the accepted cost. A conflicted activation
  loses nothing, and the loser re-issues.

## 7. Staging is invisible

Staging writes no change-log entries, does not touch the schema version, and does not move the
drop generation. The vector cache and the change feed therefore see nothing before activation,
and a `subscribe` client sees the migration as the single change it already sees.

## 8. Progress

- The dry run's `embed_rows` becomes the number of rows still to embed, so it shrinks as a
  backfill proceeds and a caller can poll with `dry_run: true`.
- A new `staged_rows` counts rows already staged for this table's current text, so the two numbers
  add up to the work.
- A migration logs one info line per 10% of rows, with namespace, table, staged and total.
- MCP progress notifications are out of scope: nothing emits notifications today, and adding an
  emitter for one operation would make the transports disagree.

## 9. Unchanged paths

These keep today's single-transaction path, because none of them embeds anything that could take
the writer for long:

- `set_vectorize … false`;
- re-vectorizing an already vectorized field, a no-op pinned by `TestNoOpVectorizeMigrationSkipsReembed`;
- every migration that does not vectorize.

The Go library has no `migrate`, so only the two engines, `/v1` and MCP change.

## Catalog format

The stage is a new catalog table, so `internal/store/catalog.go` moves `CatalogFormat` to 4.
`CatalogMinReader` stays at 3, which is only sound because of the digest invariant: a reader that
knows nothing about the stage sees a table whose `_embedding` column is entirely NULL and whose
version is unchanged, which is exactly the pre-migration state. A staged vector can never be
mistaken for a live one, because nothing outside activation writes `_embedding`.
`TestAFormat3ReaderSeesTheTableAsItWasBeforeActivation` pins that.

## Out of scope

`update` still embeds inside the write transaction (`internal/store/update.go`), and MCP progress
notifications are deferred. Both are filed separately.
