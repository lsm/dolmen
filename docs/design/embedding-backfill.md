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

Inside the single write transaction:

1. re-plan against the current schema, so a concurrent migration that landed first is seen;
2. re-check only the rows the change log shows as written since the read-side coverage check, and
   retry if any of them moved;
3. add the `_embedding` column if the table did not have one, and clear it if the change re-embeds;
4. count the rows with non-empty source text, fill `_embedding` from the stage, and require the
   number filled to equal the number counted — a row short of the count is missing or stale, so roll
   back and retry;
5. apply the steps and rebuild full-text if needed;
6. set the embedding space and dimension, bump the version, record the migration;
7. delete the table's stage.

Step 4 is where the engines differ, and the difference is worth stating rather than hiding.

**PostgreSQL computes the digest in SQL.** `sha256(convert_to(col, 'UTF8'))` has been built in since
PostgreSQL 11 and we require 16, so the fill is one statement: `UPDATE … FROM` the stage, joined on
row id, drop generation and provider identity, with `s.digest = sha256(convert_to(<source>, 'UTF8'))`
as the match. The count comparison is what makes activation exact, and the vectors that landed must
all have the same length. `sha256` is deliberately *not* added to the function allowlist `query`
enforces — these are internal statements, and a function callers may invoke is a different thing from
a function the engine uses on its own statements.

**SQLite walks the table, because it cannot hash in SQL.** It has no `sha256`, and registering a Go
SQL function would be worse: `modernc` installs a registered function on *every* connection the
driver opens, so `query` could call it on SQLite but not on PostgreSQL and the two engines would
disagree about what a caller's SQL may do. So the activation walks the table in the same keyset pages
and, for each page, confirms every row with non-empty text has a staged vector whose digest matches
that row's current text, stamping as it goes. The writer is held while it does — O(N) in-process
statements, and that is the honest cost of the digest on this engine. The per-row statements run
inside the process, so they are cheap once prepared, and memory stays bounded by a page.

Either way the fill reads the row's *current* text, so a row emptied after its page was staged ends
with no vector, and a row whose text changed after staging is caught rather than silently stamped
with the old vector.

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
- A row holds at most one staged digest per provider. Staging a vector for a row replaces whatever
  that row had staged, so a text that goes A → B → A cannot accumulate entries and cannot wedge
  activation on a stale digest.
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

The first two bullets are **not implemented yet**; they land with the wire contract in the final
slice, and nothing in the engine or on the wire promises them until then. A dry run today reports the
total number of rows to embed, as it always has.

- The dry run's `embed_rows` becomes the number of rows still to embed, so it shrinks as a
  backfill proceeds and a caller can poll with `dry_run: true`.
- A new `staged_rows` counts rows already staged for this table's current text, so the two numbers
  add up to the work.
- A migration logs one info line per 10% of rows, with namespace, table, staged and total.
  Implemented: the walk counts rows already staged, so a resumed migration reports against the whole
  table rather than against what is left.
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
