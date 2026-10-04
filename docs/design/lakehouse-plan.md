# Lakehouse engine (adapter #3) — lane plan

**Status: the order and the `query` fallback are decided; the rest is not.** The `query` question
is settled: [query-without-sql.md](query-without-sql.md) recommends an external SQL engine below
the seam, DuckDB as a separate process first, SQL passed through unchanged with its dialect
disclosed, and one engine process per namespace. **Marc answered the plan on 2026-09-29**: slice 3
is next, the `query` lockdown is pulled forward as a spike that decides slice 11, and the rest of
the lane is re-decided once that result is in. §10 records each answer with its date.

Builds on [storage-adapters-and-auth-research.md](storage-adapters-and-auth-research.md) §2 (the
per-op mapping and the three open questions), [storage-adapter-mechanics.md](storage-adapter-mechanics.md)
§1 (the per-op mechanics that are already implementation specifications), the spec in
[identity-and-engines.md](identity-and-engines.md) §0.5.1/§0.5.3/§0.6/§7/§9.3, and
[query-without-sql.md](query-without-sql.md). Adapter #2's landed state and its engine-parameterized
conformance harness are the template for every engine-shaped slice here; see
[postgresql.md](postgresql.md).

**Full text is native, and that one is settled rather than asked.** The brief for the plan asked
for full text "through the shared-Go BM25 decided in #315/#323". That decision was superseded on
2026-09-19: D27 in the spec's decision index says each engine runs its **own** native indexing and
database-side ranking, there is no shared Go scorer, and no cross-engine ranking parity.
[postgresql.md](postgresql.md) "Native search decision" records the same for adapter #2, which is
why there is no `bm25` identifier anywhere in the tree. Slice 9 plans native; Marc confirmed on
2026-09-29 that D27 settles it and no spec amendment is wanted.

---

## 1. Slices in order

**Order approved by Marc on 2026-10-03:** **4 → 5 → 6 → 11, then 7–10, 12–14**.
Spike 3 merged in [#568](https://github.com/lsm/dolmen/pull/568), and Marc chose DuckDB
(§2.8.6). Slice 11 needs real tables and committed data to query, so namespace lifecycle,
table DDL and appends precede it. The pins land with slice 4, alongside their first imports
and the version-pin test; the earlier inert-module slice stays withdrawn.

The 2026-09-29 decision pulled the confinement spike ahead of engine implementation. Its
results are recorded in §2.5–§2.8; the revised order above proceeds against those results.

Each slice is its own PR off `origin/main`, opened after the previous merges. PRs are never
stacked; merge `main` into an active branch when needed, never rebase or force-push. A behaviour change updates
`internal/conformance`, the README and `skill/`, and adds an entry under Unreleased in
`CHANGELOG.md`. Slices with code write the failing test first and push it so CI shows it red.

| # | slice | status |
|---|---|---|
| 1 | **The lane plan** | merged ([#521](https://github.com/lsm/dolmen/pull/521)) |
| 2 | ~~The pins as an inert module~~ | **withdrawn**, folded into slice 4 |
| 3 | **The harness learns a third engine** | merged ([#530](https://github.com/lsm/dolmen/pull/530)) |
| S | **The lockdown spike** (§2.5) | **done** — DuckDB confines itself; stdio cannot, so the transport is a unix socket |
| 4 | **Namespace lifecycle + catalog-in-SQLite** (pins land here) | implemented in this slice; namespace/lifecycle conformance only |
| 5 | **Table DDL + schema registry** | next after slice 4 |
| 6 | **Append + row-id allocation + idempotency** | after slice 5 |
| 11 | **`query` over the DuckDB sidecar** | after slice 6; real tables and data are prerequisites |
| 7 | **Typed reads + number normalization** | after slice 11, in the approved order |
| 8 | **Point deletes by position** | after slice 11, in the approved order |
| 9 | **Search: full text + vectors** (native, per D27) | after slice 11, in the approved order |
| 10 | **Change feed** | after slice 11, in the approved order |
| 12 | **Public selector + operator docs** | after slice 11, in the approved order |
| 13 | **`subscribe`/SSE** | after slice 11, in the approved order |
| 14 | **Compaction + maintenance** | after slice 11, in the approved order |

**What each slice still proves is unchanged** and stays in the sections below: slice 4 that a
namespace is an Iceberg catalog in its own SQLite file; slice 5 that schema evolution works; slice
6 that the id allocator and idempotency record commit atomically with their rows; slice 7 that
typed reads match the contract; slice 8 that point deletes are position deletes; slice 9 that
search is native and exact; slice 10 that the change feed is gap-free per namespace; slice 12 that
the engine is publicly selectable; slice 13 that `subscribe` works or is honestly declared
unavailable, which §9.3 permits and which `wait_for` never may be; and slice 14 that small files
get rewritten on a schedule and the maintenance op is documented — the one that bounds the
position-delete accumulation §4.2 accepts deliberately.

**Slice 8 and slice 14 are the same problem seen from two ends.** Position deletes are chosen
because iceberg-go v0.6.0 cannot read deletion vectors (§4.2), and that choice is only sound if
nothing is left to accumulate forever. So slice 8 is not shippable as a stable tier without slice
14's compaction behind it; they are sequenced apart in the table because they are separate PRs, not
because either is optional.

### The pins, which now land with slice 4

**Withdrawn as a slice of its own on 2026-09-29**; the work moves into slice 4, the first code that
imports these modules. What follows is what slice 4 has to do when it gets there.

The trial in [query-without-sql.md](query-without-sql.md) established that iceberg-go v0.6.0 does
not compile against the current Arrow: `arrow-go/v18 v18.8.0` pulls `twmb/avro v1.8.0`, whose
`SchemaNode.Root()` signature change breaks iceberg-go's `internal` package in three places. The
working combination is `arrow-go` held at **v18.6.0**.

That is a pin, and a pin with no test is a comment. The pin is not expressible as `go.mod` alone
because a routine `go get -u` anywhere in dolmen's graph wants a newer Arrow. So slice 4:

- adds `github.com/apache/iceberg-go`, `github.com/apache/arrow-go/v18` and
  `github.com/parquet-go/parquet-go` to `go.mod`, with `arrow-go/v18` held at v18.6.0 by an
  explicit `require` and a versioned `replace` so dependency updates cannot raise the effective pin;
- carries a test that fails if the resolved `arrow-go` version is not exactly the pinned one, naming
  the pin and the reason in the failure text.

Landing them together changes what the pin has to survive, for the better: because the modules are
now imported by real code, `go mod tidy` cannot drop them, and the version-pin test has something to
check. See §7 for the full pin-keeping list.

### Slice 4's namespace foundation

`internal/lakehouse` implements namespace lifecycle without claiming the complete `store.Engine`
interface or enabling public engine selection. Each namespace has its own
`<data>/<namespace>.lakehouse/catalog.db` and `<data>/<namespace>.lakehouse/data/` directory.
Hierarchical children live under the ordinary hierarchy (`project/team.lakehouse`), beside
`project.lakehouse`, so logical names such as `data` and `catalog` never collide with storage files.
The catalog is Iceberg's SQL catalog on `modernc.org/sqlite`, in WAL mode with full synchronous
commits. It holds the namespace lifetime and its own format stamp; it is separate from the data
that the future DuckDB process may read. Table DDL and mutations remain slices 5 and 6.

Creation initializes and closes the catalog in a private staging directory before renaming it
into the visible namespace. Leaf-only drop closes its handle and removes that namespace's catalog
and data together. An unpinned drop can remove a corrupt or newer catalog without opening it;
a generation-pinned drop still requires a readable, matching lifetime. Missing catalog or data files
make a namespace incomplete: listing skips it, an unpinned drop can clean up its directory, and
valid descendants still block its drop. Reopen preserves the lifetime; drop/recreate gives a fresh one. Namespace
paths, catalog files and SQLite auxiliary files reject symlinks. Operations serialize lifecycle
and catalog access, and idle handles are evicted past the configured bound (default 16).
One store owns a canonical directory in a process; deployment remains one dolmen process per local
data directory, as §2.1 declares. The foundation does not launch or link DuckDB.

`TestLakehouseNamespaceBackendConformance` runs the same namespace/lifecycle contract on SQLite
and lakehouse. The catalog smoke fixture separately verifies Iceberg catalog persistence and
isolation, Arrow append and Parquet read-back, and the manifest's real data-file size. It is a
fixture proving the pinned stack, not a lakehouse table/write API. Future writers must record every
data **and delete** file's real size and retain each row's `(data file, position)` mapping (§2.8.2).

### Slice 3 in more detail

`store.ValidateEngine` ([internal/store/engine.go](../../internal/store/engine.go)) is a two-case switch;
`internal/conformance/engine_test.go` resolves `DOLMEN_ENGINE` through it and
`internal/blackbox/harness_test.go` has its own resolver. Slice 3 adds the third name to
`ValidateEngine`, the `"lakehouse"` case to both resolvers, and — the point of the slice — the
suite's refusal to open it: `openEngineStoreTraced` gains a branch that returns a teaching error
naming the slice that lands the engine. Every existing test stays green, which is the proof that
the third engine cost nothing yet. It also splits the skip helper, and the reason is a name that has
come to mean two things. `sqliteOnly` ([internal/conformance/engine_test.go](../../internal/conformance/engine_test.go))
skips whenever the active engine is not SQLite, and its call sites are not all the same kind of
skip: some probe SQLite **storage internals** and could never run on a different engine, while
others encode a fact about SQLite's **grammar** — the extended-FTS syntax adapter #2 also refuses —
which is a per-engine decision rather than a storage fact. Two engines is why that has gone
unnoticed. A third engine turns it into a live hazard in the direction the name hides: a
Postgres-specific skip written as "not this engine" silently widens to skip on the lakehouse too,
and a lakehouse gap behind a test that was always meant to be about Postgres reads as coverage
that passed. Slice 3 splits it into helpers named for the **reason** — one for storage-internal
probes, one per engine's extended-grammar support — so a call site has to say which it is, and no
test's meaning changes.

---

## 2. The DuckDB process

### 2.1 One process per namespace, not pooled

**One DuckDB process per namespace**, and the reason is §0.5.3 rather than throughput. Confinement
must make cross-namespace reference *impossible by mechanism*, and the mechanism
[query-without-sql.md](query-without-sql.md) names is "the engine has nothing else to reach". A
pool with several namespaces attached to one process breaks that: a query in process P can reach
whatever P has attached, so the wall is a policy check inside a shared engine rather than a
structural fact. That is exactly the mechanism the spec rejects, and it is strictly weaker than
what adapter #1 gets from the file.

It is also cheaper than it sounds, because it inherits adapter #1's existing handle discipline:
`store.Store` already opens at most `-max-open-namespaces` namespaces at once
(`DefaultMaxOpenNamespaces = 128`) and closes the least recently used unpinned one past that, and
`TestEveryOperationReleasesItsNamespace` catches a missing unpin. The lakehouse engine reuses that
shape: a namespace handle owns a DuckDB process, and the same LRU closes it.

One honest caveat on that reuse. `-max-open-namespaces` is currently a **SQLite-engine** knob —
its help text says so, and it is absent from adapter #2's surface because Postgres holds
connections in a pool rather than in per-namespace handles. Reusing it for a *process* count is a
reuse in name only: the unit changes from an open file handle to a running process with its own
memory, so the same default means something quite different. The plan keeps the knob and the LRU
shape but expects the default to be **much lower** for this engine, and says so in the flag's help
text, because a default of 128 processes is a different proposition from 128 file handles. The
costs are the ones that follow: process startup is milliseconds-to-tens-of-milliseconds, so a cold
namespace's first `query` pays it; and each process holds its own memory.

**Topology.** §0.6 makes deployment topology an **engine-declared** property, and for this engine
the declaration is **one dolmen process per data directory**, the same topology SQLite has. Two
dolmen processes over one namespace directory would be two catalogs over one Parquet tree with no
coordination between them, and Iceberg's optimistic concurrency would surface the conflict as
`409` rather than as corruption — correct, but a poor answer to a question a declaration would
have answered up front.

There is a gap here the plan has to own rather than assume away. §0.6 says *engine-declared*
without naming a channel, and `EngineCapabilities`
([internal/store/engine.go](../../internal/store/engine.go)) has six fields — `vector_execution`,
`ann_recall_bound`, `notifications`, `subscribe`, `query_dialect`, `filter_dialect` — **none of
them topology**, and adapter #1 declares nothing there. So the lakehouse's topology is currently
expressible only in prose, and making it machine-readable means adding a capability field, which is
a contract-surface change: the `capabilities` op schema, the OpenAPI output, the MCP tool surface,
and the conformance suite. None of that exists for adapter #1 or #2, so this is **new work with no
precedent in the tree**, and it is not budgeted by any slice below. §10 Q11 carries it, and the
plan's working assumption is the one both existing engines already take: prose, in the operator
documentation.

### 2.2 How it starts, and how it is supervised

A namespace handle starting means: resolve the DuckDB binary, spawn it, wait for readiness, and
register it with the supervisor. Talking to it is one of three mechanisms, in descending
preference:

1. **A `duckdb` process driven over stdio with the DuckDB CLI's JSON protocol** — a
   newline-delimited request/response stream. Simple, no port, no auth surface, and the process's
   stdin/stdout is a private pipe to one dolmen process, which is part of why confinement holds.
2. **A child `duckdb` server on a unix socket or an ephemeral loopback port**, chosen with an
   ephemeral port so two namespaces never collide. Adds a listener and therefore a thing to
   restrict.
3. **A `go-duckdb` driver over the C API** — **rejected**. It is cgo, and the binary is
   `CGO_ENABLED=0` with no cgo dependency permitted. Named here so the option is visibly closed
   rather than merely unmentioned.

Mechanism 1 **is withdrawn** — the spike in §2.5 found that no CLI mode refuses a dot-command, and
the reasoning that follows is the reason it was ever in doubt. The CLI is line-oriented, so a line
beginning with `.` is a command rather than SQL, and `.shell`/`.system` run an arbitrary program.
Measured on v1.5.6: a single statement followed by a newline and `.shell <cmd>` executes the
command, under a configuration that stops every filesystem escape, and no mode prevents it — not
plain stdin, not `-json`, not `-c '<string>'` (a single argv string with no framing at all). There
is no safe mode, no flag, and no separator. A stdio transport would therefore hand a caller
arbitrary code execution through a newline, which is worse than the confinement failure it was
meant to avoid.

**Mechanism 2 — a child `duckdb` server on a unix socket — is the plan.** It speaks a real protocol
with a real message boundary, so the statement is a value in a framed message rather than a line in
a stream, and there is nothing for a newline to escape into. The confinement settings in §2.4 stand
unchanged and are now measured rather than assumed: they are what stops the *contents* of a query
from reaching outside the namespace, once the process itself is reachable only by dolmen.

**Supervision** is the same shape `cmd/dolmen` already has for its own lifecycle: a start, a
readiness wait bounded by a context, a liveness check, and a shutdown path that closes admitted
work before the child is signalled. Four specific behaviours:

- **Readiness is a real handshake**, not a sleep: the spawn is not returned until the process
  answers a trivial statement. A namespace handle that is returned before the child is up would
  fail its first `query` for a reason that has nothing to do with the query.
- **Restart on death, bounded.** If the child exits while the namespace is open, the supervisor
  respawns it up to a small retry budget with backoff, because a crashed query process is
  recoverable and a namespace that is permanently down is not. Past the budget the handle marks
  itself failed and every operation on it answers with a teaching error naming the namespace, the
  child, and the flag that configures the path — not a bare `internal_error`.
- **No crash loop.** An engine whose child dies instantly, every time, is a configuration error
  (a wrong path, a missing shared library), and retrying it forever hides that. The budget
  distinguishes "the child died under load" from "the child cannot start".
- **Shutdown is ordered.** `Close` stops accepting new operations, waits for admitted ones —
  including in-flight statements, which are cancelled by closing the connection — and only then
  signals the child, in the existing `-shutdown-grace` discipline. The child is a process dolmen
  started, so dolmen owns reaping it; a leaked child per namespace would be a real leak on a
  laptop.

**The error class.** A sidecar that is down is a new failure mode and it needs a class. The
taxonomy ([internal/derr](../../internal/derr)) has ten codes and none fits: `embedder_unavailable` is
about a *different* subsystem, and reusing it would tell a caller to configure an embedder.
[query-without-sql.md](query-without-sql.md) flagged this as not yet existing. §10 Q3 asks Marc to
name it; the plan's recommendation is a new `sql_engine_unavailable` code alongside
`embedder_unavailable`'s shape, with `query_error` reserved for SQL the engine itself rejected —
because those are different problems with different remedies, and §2's error contract teaches the
remedy.

### 2.3 Resource limits

A child process is a resource boundary the operator can see, which is the one thing the single
static binary gives up. Three limits, all set at spawn, all documented:

- **Memory** — `duckdb`'s own `memory_limit` setting, passed as a process argument, defaulted from
  a new flag with the same convention as `-vector-cache-size` and `-max-namespace-size`
  (`-duckdb-memory`, `DOLMEN_DUCKDB_MEMORY`, a byte string like `1GiB`, `0` for DuckDB's default).
- **Concurrency** — a per-namespace cap on concurrent statements, defaulting to something small
  (one DuckDB process is single-writer by nature; the cap bounds concurrent readers and the memory
  each costs). Pinned as a flag rather than hardcoded, like the other limits.
- **Process count** — bounded by `-max-open-namespaces`, the knob that already decides how many
  namespaces are held at once, with a **much lower default on this engine** for the reason in §2.1:
  the unit is a running process, not a file handle. One knob, two effects, and the README says so.

None of these is a new concept in this codebase; all three follow the existing flag conventions.
What is new is that they are ceilings on a **child process**, so the failure mode is a killed
child (a `query` error naming the limit) rather than a refused request. The error text has to say
which, which is why the limit is named in it.

### 2.4 What the child is given, and what it is not

**Decided 2026-09-29 (Q4): DuckDB's own settings are the only guard.** There is no statement filter
in dolmen — not as a second layer, not as a first one. The plan originally left room for one and
asked; the answer is no, which keeps §0.5.3's "impossible by mechanism" honest, because a filter
dolmen applies is precisely the mechanism the spec does not accept. Confinement is either the
process's own configuration or it is nothing.

The process is handed **exactly one namespace**: its Parquet/Iceberg **data directory**, and
nothing else. The distinction between the data directory and the namespace directory is the load-
bearing one, and it is easy to get wrong: a namespace also has a SQLite file holding the catalog,
the commit log, cursors and idempotency records, and the child is **not** given that. Concretely,
it is *not* given:

- the data directory root, so it cannot see another namespace's directory;
- the namespace directory itself, so it cannot read the catalog, the commit log, the cursor table
  or the idempotency table;
- any path outside its own data directory, so a `query` cannot reach the grants registry or the
  model cache;
- the file-writing capability beyond its own data directory, so a caller's `COPY ... TO` cannot
  write outside it.

**Measured on v1.5.6 (§2.5): DuckDB can confine itself to one data directory, completely.** The
settings that do it, and one correction that matters more than the list:

- `enable_external_access = false` — **this is the guard.** `allowed_directories` on its own
  confines nothing: set it and leave external access on, and `read_csv_auto` reads a sibling
  namespace's file, the namespace's own SQLite catalog, and a file behind a symlink planted inside
  the allowed directory. `allowed_directories` is a *widening* knob — it names what stays reachable
  while external access is off — not a narrowing one. Read it the other way round and the sandbox
  does not exist.
- `allowed_directories` — the one namespace **data** directory, and nothing else. This is what
  re-permits the engine to read its own Parquet once external access is off. Both settings are
  required; the second alone is not a sandbox.
- `autoinstall_known_extensions = false`, `autoload_known_extensions = false`,
  `allow_persistent_secrets = false` — no extension can be fetched or loaded, and no secret is
  persisted across restarts.
- `lock_configuration = true` — set **last**. The ordering is forced, and it is the reverse of the
  obvious one: `allowed_directories` **cannot** be set once `enable_external_access` is false
  (`Cannot change allowed_directories when enable_external_access is disabled`), so the list goes
  first and external access is closed second. External access can be turned off by a `SET` on an open
  connection but never turned back on, and nothing can be changed once the configuration is locked,
  so the whole sequence has to run before any caller SQL does. **How that is spelled depends on the
  process, and getting it wrong fails open silently: `.duckdbrc` is read by the `duckdb` shell and
  *not* by an embedded library.** §2.5 measured this against the CLI, where a `.duckdbrc` under a
  per-namespace `HOME` is the mechanism. §2.8.3 measured the same lock inside a sidecar, where
  `allowed_directories` and `access_mode` are `DBConfig` fields at open time and the rest are `SET`
  on the connection, ending with `lock_configuration`. A sidecar that writes a `.duckdbrc` looks
  configured and is not.

Every attack the table below lists is blocked, measured rather than assumed:

| attack | result |
|---|---|
| `ATTACH` a sibling namespace's database | blocked |
| `read_csv` on a sibling namespace's file | blocked |
| `read_csv` on the namespace's **own SQLite catalog** | blocked |
| `read_csv` through a symlink planted inside the allowed directory | blocked |
| `..` traversal out of the directory | blocked |
| `COPY ... TO` / `COPY ... FROM` outside | blocked, and nothing is written |
| `COPY ... TO PROGRAM` (shell out) | blocked |
| `read_parquet` / `read_text` / `glob` outside | blocked |
| `INSTALL` / `LOAD` of an extension | blocked |
| http(s) URL | blocked |
| `SET enable_external_access=true`, `SET allowed_directories=['/']`, `RESET lock_configuration`, a shadowing `SET VARIABLE` | blocked; the settings read back unchanged |
| **CLI dot-commands** — `.shell`, `.system`, `.output` from a statement plus a newline | **not blockable by any setting**; see §2.2 |

The dot-command row is the one gap, and it is not a gap in the settings: it is not a filesystem
operation at all, so no filesystem setting reaches it. It is a property of the transport, and it
is why §2.2's mechanism 1 is withdrawn.

**The dot-command case decided the transport, and it is now settled (§2.5).** The DuckDB CLI is a
line-oriented client: a line beginning with `.` is a command, not SQL, and `.shell` and `.system`
run an arbitrary program. A caller writing `SELECT 1;` and a newline and `.shell <cmd>` runs that
command — measured, under the full lockdown, in every CLI mode. So the answer to "can the stdio
protocol make it impossible" is no, and §2.2's mechanism 2 is the transport.

### 2.5 The lockdown spike — result

**Run 2026-09-29 against DuckDB v1.5.6 (Variegata) 069cc9f9b5.** It lived in
`internal/duckdblockdown` — a minimal helper that starts a CLI locked to one namespace's data
directory, plus the tests that attack it. The tests are the deliverable; the helper exists only so
there is something to attack. It is deliberately **not** engine code: nothing is wired into
`store.Engine` and no operation reaches it.

1. **Can DuckDB confine itself to one directory by its own configuration? Yes**, completely. Every
   escape in §2.4's table is blocked, including the namespace's own SQLite catalog and a symlink
   planted inside the allowed directory. One correction matters: `enable_external_access=false` is
   the guard, and `allowed_directories` widens rather than narrows. Slice 11 proceeds as
   pass-through SQL, and **Q2's fallback chain is not triggered**.
2. **Can the stdio protocol make CLI dot-commands impossible? No.** `.shell`, `.system` and
   `.output` all run, under the full lockdown, in every mode tried — plain stdin, `-json`, and
   `-c '<string>'`. A caller reaches them with a statement, a newline, and a dot-command. So
   §2.2's mechanism 1 is withdrawn and mechanism 2 is the transport.

The result is pinned as a test, not only as prose: `TestEveryCLIModeRunsADotCommand` **asserts that
the dot-command does run**, so if a future DuckDB makes a mode refuse it, that test fails — which is
good news, because it means a stdio transport may be confinable and mechanism 1 worth
re-evaluating. The confinement tests assert the opposite direction, that every escape is refused,
and the settings are read back with `current_setting` rather than assumed to have been applied.

**There is deliberately no dot-command detector in this package, and none should be added.** Q4
rules a statement filter out even as a second layer, and the unix-socket transport makes one
unnecessary; a helper that sniffed caller SQL for a leading `.` would be precisely the mechanism
the decision rejects, and an unused one is an invitation to wire it in. The transport's safety is
shown by the attack tests — they run the payload against the real CLI and observe what happens —
not by a predicate that guesses. Reintroducing filtering needs the Q4 decision reopened first.

**Untested, named rather than assumed away.** The CI job is Linux-only, so:

- **Windows drive letters and UNC paths** (`C:\`, `\\server\share`) are unproven, and `\\?\` and
  `\\.\` device paths are a separate surface. A POSIX prefix check says nothing about any of them.
- **macOS** is verified only by the run above (osx/arm64), not by CI.
- Whether the `.duckdbrc`-under-`HOME` startup survives a DuckDB upgrade. It is the documented
  mechanism, but it is a config file rather than a flag, so an upgrade that changed precedence would
  change confinement silently. The CI job asserts the settings actually took effect, which is what
  catches that.

### 2.6 Q5 compared: in-process pure Go against the DuckDB sidecar

**Measured 2026-09-29, both against the same attacks.** The in-process engine is
`spike/inprocess` (a nested module, so a "no" leaves dolmen's `go.mod` and its binary
byte-identical); the sidecar is §2.5's `internal/duckdblockdown`. This is the evidence Q5 asked
for, and it changes Q5's shape: the in-process option is no longer a live alternative, so the
question is not "which engine" but "is a second released binary acceptable".

| | **go-mysql-server, in process** | **DuckDB, child process** |
|---|---|---|
| **pure Go** | **yes** — `CGO_ENABLED=0` builds, no `runtime/cgo`, no `CgoFiles` in the graph | no — needs a separate binary, which is the point |
| **file read/write escapes** | **open.** `LOAD_FILE()` returned a file's bytes and `INTO OUTFILE` wrote the table's rows to disk, both under `IsReadOnly` + `IsServerLocked`, and **no `SET` closes either** — `secure_file_priv` is a read-only variable and there is no equivalent knob | **closed.** every path refused, with the failure proven to be the sandbox's |
| **cross-namespace reads** | **open in-process.** a registered database is reachable by name; `SHOW DATABASES` and `information_schema.schemata` enumerate every namespace, violating §0.5.2's existence hiding | **impossible.** the process only ever sees one data directory |
| **one engine per namespace** | possible, but not with a shared analyzer — reusing one panics (`get_lock` is already registered), so it means an analyzer each. The in-process reachability below is unaffected either way | one process per namespace, as §2.1 requires |
| **binary cost** | **+61 MiB, +131%** — two minimal mains, one empty (1,815,330 bytes) and one importing go-mysql-server (66,288,882), so dolmen's 46.8 MiB binary would reach ~108 MiB. CI recomputes it | +19 MB, and external |
| **packaging** | nothing to ship | helper binary, second release artifact, an SBOM that cannot describe it, a distroless change (§3) |
| **dialect** | `mysql` — legal under D28 by disclosure, but a third branch in `skill/dolmen.md` and a new portability story for callers | `duckdb` — also a third value, same shape |
| **type mapping** | lossy where MySQL is: a dolmen `boolean` reads back as `int8`, so `internal/value` needs a mapping layer | DuckDB's `BOOLEAN` is a real bool |
| **Parquet-backed table** | works — scanned and filtered through the engine | not applicable; DuckDB reads Parquet natively |
| **transport** | n/a | **needs a wrapper we would build and ship** — see below |

**The in-process engine loses on the one axis the lane exists for.** §0.5.3 makes confinement an
engine obligation and Q4 rules out a statement filter, so an engine that cannot confine by
construction cannot be made to conform at all — and this one cannot: the file escapes are open and
unclosable, which is not a gap to be closed later but the library's shipped behaviour. Its
per-namespace reachability is the deeper version of the same problem: the plan's premise for the
sidecar is "the engine has nothing else to reach", which is true of a process and false of an
in-process engine sharing one catalog.

**What the in-process option does buy, recorded so the trade is not overstated**: it is the only
candidate that is pure Go, and a subpackage boundary would keep the +61 MiB off programs that do
not use the engine. If confinement were closable it would still be a large cost for a large gain —
which is why the measurement mattered rather than the assumption.

### 2.7 The DuckDB socket does not exist in the stock release

Mechanism 2 (§2.2) was never run in §2.5, and the answer is that **it cannot be, without a
wrapper dolmen builds and ships**:

- **The stock `duckdb` CLI has no listener.** No `-listen`, `-socket` or `-port` flag; it speaks
  line-oriented stdio, which is exactly what §2.5 proved unsafe.
- **One extension claims a server protocol — `quack`, "The DuckDB 'Quack' Client/Server
  Protocol" — and it is not obtainable.** `INSTALL quack FROM community` returns HTTP 404 on
  `linux_amd64`, `linux_arm64`, `osx_arm64` and `windows_amd64`, while published extensions on the
  same host return 200. It is registered in the extension list but unpublished.
- **And loading it would not fit the lockdown anyway.** Under §2.4's settings, `LOAD` of an
  extension that is not installed **silently succeeds and does nothing** — the statement passes,
  the next statement runs, and no server appears. `INSTALL` is refused outright, because the
  extension directory is outside the allowed data directory.

So the choice for slice 11 is narrow and worth stating plainly: **a custom wrapper, shipped and
released per platform** (six platforms, a second artifact, the SBOM gap of §3, and a component dolmen
must then keep in step with DuckDB's own release cadence), **or a stdio transport with a
newline-escape hole in it**, which Q4's "DuckDB's own settings are the only guard" makes
unacceptable because no setting reaches a dot-command. The middle option does not exist in v1.5.6.

**What stays untested here:** a future DuckDB may publish a server-protocol extension, or the CLI
may grow a listener flag. The `duckdb-lockdown` CI job does not currently assert either, and a
change there would not fail a build — so re-check this before slice 11 rather than trusting it to
stay true.

### 2.8 Spike 3 — DuckDB against DataFusion as the sidecar's engine

Marc's direction (2026-10-02) settles the shape and leaves the engine open: the lakehouse ships
`query`, it runs in a **sidecar process** that dolmen talks to over a pipe, and dolmen itself stays
`CGO_ENABLED=0`. Linking an engine in with cgo is rejected — an engine crash or a memory-safety bug
would take down or expose the whole server. The open question is which engine the sidecar runs.

Two minimal sidecars, both outside dolmen's `go.mod` so the loser can be deleted without touching
the main graph, each with its own CI job:

| | path | language | lines | module boundary |
|---|---|---|---|---|
| DuckDB | `spike/sidecar-duckdb/` | C++ | 469 | links prebuilt `libduckdb` |
| DataFusion | `spike/sidecar-datafusion/` | Rust | 382 | `spike/sidecar-datafusion/Cargo.toml` |

Both speak one deliberately trivial protocol (`spike/driver/PROTOCOL.md`): tab-separated framed
requests on stdin, framed rows and errors on stdout. Not the final wire — no streaming, no
pagination, no cursor, no parameters. It exists so the two engines are measured *through the same
driver and the same attacks* rather than two different protocols.

One Go driver (`spike/driver/`, nested module) writes the fixture, starts either sidecar, and runs
the battery. `SIDECAR_BIN` and `SIDECAR_ENGINE` select the engine, so both CI jobs run byte-identical
tests.

#### 2.8.1 What is already settled, and it is not what the plan assumed

**DuckDB needs no source build, and never did.** §2.7 recorded a source build with
`EXTENSIONS='iceberg;httpfs'` as the fallback. That fallback is not needed:

- Prebuilt `libduckdb` is published for `linux-amd64`, `linux-arm64`, `osx-universal` and
  `windows-amd64` — every platform in §3's `PLATFORMS` list that DuckDB targets.
- `iceberg` and `httpfs` are published prebuilt for **all five** platform tokens
  (`linux_amd64`, `linux_arm64`, `osx_amd64`, `osx_arm64`, `windows_amd64`) — verified HTTP 200 on
  each, against a host that returns 404 for `quack` (§2.7). `spike/fetch-sidecar-deps.sh` pins all ten
  digests. CI builds and runs four of the five; `darwin-amd64` is pinned but has no packaging leg.
- **A pre-placed extension file loads with no network at all**, which is the property that matters:
  the extension directory is populated before the process starts and `LOAD iceberg` reads from disk.

So the DuckDB packaging cost is: download two artifacts, compile about 470 lines of C++, ship the
extension files alongside. No C++ toolchain beyond a compiler, no 40-minute DuckDB build, no
`ccache`, no C++ in the release pipeline beyond one small `g++` invocation. **Measured:
seconds and about 100 KB** for the shared-library build on `linux-amd64`.

**A fully static Linux sidecar is *not* available, and this corrects an assumption rather than
confirming one.** The `-musl` zips do ship `libduckdb_static.a` (83,882,636 bytes at v1.5.6) for both
linux architectures, but linking it fails, for two independent reasons:

- **`libduckdb_static.a` is missing `duckdb::ExtensionHelper::LoadAllExtensions(duckdb::DuckDB&)`** —
  an undefined reference from the library itself, so the archive is incomplete for embedding.
- **It needs a musl toolchain, not glibc's.** Linking it with `g++ -static` on ubuntu-latest fails on
  `res_init`, glibc's NSS resolver entry point, and warns that `getaddrinfo` in a statically linked
  application "requires at runtime the shared libraries from the glibc version used for linking" —
  which is the static link failing in the way static glibc links fail.

**So the DuckDB sidecar ships as a binary plus `libduckdb.so` plus the two extension files — four
artifacts, one of them 72 MB — and it is not fully static on any platform.** That is a real cost
against §3's distroless runtime stage and against a single-file release, and it is the sharpest
packaging difference between the two candidates. `sidecar-packaging` records the exact failure
rather than hiding it, so a future DuckDB that fixes the archive would show up as a changed line.

**The DataFusion dependency set closes, and the way it closes is worth recording.** Taking
`datafusion` and `iceberg` independently produces two incompatible Arrow versions and a wall of
type errors — that is a pinning mistake, not an upstream blocker, and an earlier draft of this
spike mislabelled it as one. The correct pin is the integration crate's own:

- `iceberg-datafusion` 0.10.1 depends on `datafusion` **53.1.0** and `iceberg` 0.10.x.
- At that pin the graph holds **exactly one Arrow (`arrow-array` 58.4.0) and one parquet (58.4.0)**.
  `cargo tree -i arrow-array` shows a single version, and the `sidecar-datafusion` CI job *asserts*
  it and fails the build if a future bump splits them.

#### 2.8.2 Three traps worth writing down, because all three look like success

**The engine writes to your response channel.** The brief specifies framed rows on stdout, and
DuckDB writes its own logging to stdout — `Loading extension iceberg from ...` — which desynchronises
the framing. It does not fail loudly: it **misattributes one request's answer to another**, so a
refusal test reads as a success and a success test reads as a refusal. The first CI run of this spike
produced exactly that, and the symptom was a battery where *everything* looked refused. Two fixes,
both kept: the DuckDB sidecar sets `log_config.enabled = false`, and the driver skips any line that
does not carry the request id rather than failing on it, bounded so a silent sidecar still fails.
There is now a test asserting the response channel carries nothing else.

**A sidecar that logs to stdout cannot share stdout with its host.** This is not a spike artefact. A
real wire should either put responses on a separate descriptor or length-prefix the frames, and
either way the sidecar must own its output. Recorded because it is cheap to get wrong and expensive
to debug: the failure mode is wrong answers, not an error.

**DuckDB's `iceberg_scan` ignores `version` when given a numeric snapshot id.** `version` is a
*tag* parameter; pass it a snapshot id and the scan silently reads the **current** snapshot. A
reader's time-travel test then passes at the current snapshot and fails only if it happens to
compare counts. The correct option is `snapshot_from_id => <id>`, which was verified against a
three-snapshot fixture: 50 rows seeded, 51 after the late arrival, and 49 once the two position
deletes commit, with the unpinned read matching the current one. An earlier draft recorded 50 / 51 /
51, which was the fixture bug of §2.8.2 (the deletes aimed at the wrong data file) and not the
engine. **This is the kind of hole the confinement battery exists to catch, in the pinning
direction instead of the escape direction.**

**DataFusion has no cooperative per-query cancel.** There is no `duckdb_interrupt` equivalent, so
the sidecar holds the `tokio::task::JoinHandle` and calls `abort()`. That is strictly coarser than a
cooperative interrupt: it cancels the task, not the scan, and anything already committed to a file
handle is left as it is. The `lifecycle` tests measure both engines the same way — cancel
mid-flight, then kill-on-timeout, then a fresh sidecar — so the difference is on the record rather
than assumed.

**A position delete that names the wrong file deletes nothing, and every other check still passes.**
Not an engine finding — a fixture bug — and worth recording anyway because it is the shape of bug
this fixture exists to catch. Iceberg names data files after a uuid, so the lexicographically first
`data/*.parquet` has nothing to do with write order. The first CI run aimed the delete file at the
*late-arrival* file, whose single row made positions 2 and 5 out of range, so the deletes silently
did nothing; the counts stayed plausible and only one assertion caught it, and only by accident of
uuid ordering. The fixture now picks the data file **by reading the candidates and finding the one
that holds the deleted id**, with a fixture test pinning it. The lesson for slice 4 is that a
position delete is `(data file, row position)`: a writer that cannot name the data file cannot
delete the row, and nothing downstream will tell it.

#### 2.8.3 Confinement: how each sidecar is locked

**`.duckdbrc` is a CLI feature, and an embedded DuckDB silently ignores it.** This is the most
important correction in the spike, because §2.4's plan writes the confinement into a rc file. The
first CI run did exactly that and **all 24 escape attempts succeeded**. The `duckdb` shell reads
`.duckdbrc`; a library opened with `DuckDB(path, &config)` never does, so nothing warns and the
process looks configured when it is not. `DBConfigOptions::unrecognized_options` is not a way in
either: the process aborts with `The following options were not recognized: ...`.

**`enable_external_access = false` is about files, not about writing.** With only that guard, an
in-memory catalog accepted `CREATE TABLE`, `INSERT`, `UPDATE`, `DELETE` and `DROP VIEW`. That last
one matters beyond tidiness: a sidecar serves every caller of a namespace, so a caller who can
replace the `events` view can change what the next caller reads. Refusing writes needs a second,
different mechanism, `access_mode = READ_ONLY`, and **a read-only database cannot hold the views
`init` creates**. The first attempt opened read-only and then created the views, which DuckDB
refused; every query then answered "table does not exist", and half the battery's refusals were
really that.

So the DuckDB sidecar runs in three phases, and caller SQL only ever reaches the last:

1. `init` records tables and checks the pinned snapshot with `iceberg_snapshots()`. No SQL from the
   caller runs.
2. The first query **seals** the sidecar: it writes a private catalog file, read-write, holding one
   view per table, closes it, and reopens it with `access_mode = READ_ONLY` and
   `allowed_directories = {namespace dir}` as `DBConfig` fields. It then runs `LOAD iceberg` and
   finally `SET enable_external_access = false`, the autoload, autoinstall and secret flags, and
   `lock_configuration = true`. The sidecar exits non-zero if any of these is refused.
3. Queries run on a worker thread against one connection, so `cancel` reaches
   `Connection::Interrupt` while a query is in flight.

**DataFusion's lock is `SQLOptions` plus what the sidecar does not register.**
`with_allow_ddl(false).with_allow_dml(false).with_allow_statements(false)` refuses DDL, DML and
`SET`. Nothing reads a path or URL because the sidecar never registers a function or table that
does, and `enable_url_table` stays off.

#### 2.8.3b A test that proves nothing looks exactly like a test that passes

The first battery run reported `read_csv`, `read_text` and `glob` as escapes. They were not: **the
fixture had planted the secret *inside* the namespace's own data directory**, which is the one
directory a confined sidecar is supposed to be able to read. The battery was aiming at the one path
that was always allowed.

The fix generalises to a rule worth keeping:

- **Every attack must target something genuinely outside the allowed set.** The fixture now plants
  `secret.txt`, `secret.csv` and `secret.parquet` in a *sibling* directory, and a fixture test pins
  that they are outside.
- **Every attack must be one that would succeed if unconfined.** `read_parquet` on a text file fails
  with `No magic bytes found at end of file`, which is not a refusal — it is a reader complaining
  about a file, and scoring it as a block is how a battery once reported green while every escape
  worked. The `.parquet` decoy is now a real Parquet file carrying the marker.
- **A test must also assert that nothing was written outside.** The battery checks that no new file
  appeared in the outside directory, because `COPY ... TO` and `ATTACH` are escapes that a
  return-value check alone can miss.
- **There is a positive control.** `TestTheUnlockedSidecarCanActuallyReadTheSecretSoTheComparisonIsReal`
  asserts the unlocked sidecar *does* read the secret. Without it, "refused under the lock" could
  just mean the attack was malformed — which is precisely what happened.

The interesting question is not *whether* an escape is refused but **what refused it**. A refusal
that also happens with the lock off is an engine *default* — it holds today and nobody owns it; a
refusal that appears only with the lock on is the *mechanism*, and it is ours.

So each sidecar is started twice, once with `SIDECAR_UNLOCKED=1`, and the driver runs the same
24-statement battery against both. Three outcomes, and each is a different finding:

| outcome | meaning |
|---|---|
| refused locked, **allowed unlocked** | the **mechanism**. Ours, and it is load-bearing. |
| refused locked **and** unlocked | an engine **default**. Holds, but nobody owns it. |
| **allowed locked** | a hole. Fails the build. |

For DuckDB the mechanism is §2.4's lock, applied through `DBConfig` rather than a rc file
(§2.8.3): `allowed_directories` first, then `enable_external_access = false`, then
`lock_configuration = true` last. For DataFusion
the mechanism is `SQLOptions::with_allow_ddl(false).with_allow_dml(false).with_allow_statements(false)`.

**Measured, the two split the battery very differently, and the split means different things.**

| | refused by the lock | refused with the lock off too | allowed under the lock |
|---|---|---|---|
| DuckDB | **14** | 10 | **0** |
| DataFusion | **3** | 21 | **0** |

- **DuckDB's 14** are the file, URL, `ATTACH`, `COPY`, `INSTALL`, `LOAD` and `SET`/`PRAGMA`
  attacks: everything `enable_external_access` and `lock_configuration` exist for. Its 10
  "defaults" are `getenv` (absent), `CREATE EXTERNAL TABLE` (not DuckDB syntax), the two URL tables
  (no `httpfs` loaded in the unlocked run either), the other namespace's directory (not readable
  unlocked in that run), and DML against `events`, which is a view and so refuses writes even
  unlocked. The read-only catalog refuses them too; this battery can't separate the two.
- **DataFusion's 21 are not defaults in DuckDB's sense.** They are refused with the lock off
  because the capability is not in the session at all: the sidecar registers no file-reading table
  function, no `ATTACH`, no extension loader, no environment access. No SQL can turn them on; only
  a change to the sidecar's Rust code could. The positive control is skipped on DataFusion for the
  same reason, since even the unlocked sidecar has no way to read the secret. **So DuckDB's
  confinement is a set of switches locked off, and DataFusion's is a smaller surface with nothing
  to switch.** Both held all 24 under the lock.

#### 2.8.4 Measured by CI, not by assertion

Every number below comes from the CI job logs of #568 (linux amd64 unless stated). Neither the
laptop nor ai0 was used for timing.

| | DuckDB | DataFusion |
|---|---|---|
| read-back at the pinned snapshot, position deletes applied | **yes** | **yes** |
| read-back at the older snapshot | **yes** | **yes** |
| refuses a snapshot the table does not hold | **yes** | **yes** |
| escape battery, 24 statements, allowed under the lock | **0** | **0** |
| cancel mid-flight | **yes**, ~150 ms (`Connection::Interrupt`) | **yes**, ~150 ms (task abort) |
| kill on timeout, then a fresh sidecar | **yes** | **yes** |
| a query that needs to spill under a memory ceiling | **not shown** (see below) | completed a 3M-group `GROUP BY` under 64 MB |
| 10M rows: `count(*)` | **11 ms** | 752 ms |
| 10M rows: `sum` | **65 ms** | 213 ms |
| 10M rows: selective filter | **27 ms** | 50 ms |
| 10M rows: `GROUP BY` | **230 ms** | 396 ms |
| 10M rows: join on `id` | **334 ms** | 834 ms |
| start to first answer, 1,000 rows | 453 ms | **11 ms** |
| sidecar binary, linux amd64 | 106,656 B | 114,403,912 B |
| what ships beside it | `libduckdb.so` 70,546,800 B, `iceberg` 50,827,374 B, `httpfs` 21,580,734 B | nothing |
| shipped total, linux amd64 | **≈136 MiB in 4 files** | **≈109 MiB in 1 file** (stripped) |
| Linux fully static | **no**: `libduckdb_static.a` is incomplete (§2.8.1) | **yes** |
| build, linux amd64 | **seconds** | 683 s |
| build, windows amd64 | **10 s**, 355 KB | 1002 s, 122 MB |
| toolchain | a C++ compiler | a Rust toolchain; MSVC's `link.exe` on Windows |
| Arrow versions in the graph | n/a | **1** (`arrow-array` 58.4.0), asserted in CI |
| memory safety of the engine | C++ | Rust |

Notes on reading it:

- **DuckDB is 1.7–3.3× faster on every shape except `count(*)`, where it is about 70×**: it
  answers a count from Parquet metadata, while iceberg-rust's scan reads the column. The DataFusion
  build is `opt-level = 2`, no LTO, default `target_partitions`, and nobody tuned the iceberg-rust
  scan, so the gap is an upper bound. An earlier run reported 3–6×; its fixture had zeroed 80 rows
  and runner timings vary between runs, so treat the ratios as rough.
- **DuckDB spilling under the lock is not shown.** At 64 MB DuckDB refused a 3M-group `GROUP BY`
  with *Out of Memory*, locked and unlocked alike, so that ceiling is below what it needs to work at
  all. From 96 MB to 384 MB, a 10M-group `GROUP BY` and a `row_number()` over all 10M rows both
  completed, but they also completed with `use_temporary_directory = false`, so that control did
  not take effect and nothing here shows whether a spill happened. The sidecar gives DuckDB a
  private temp directory inside `allowed_directories`, because the default one sits outside it and
  the lock would otherwise block spilling. Slice 11 has to show a spill with a control that works,
  before the memory ceiling is advertised.
- **DataFusion starts 40× faster**: 11 ms to a first answer against DuckDB's 453 ms, most of which is
  sealing the catalog and loading the `iceberg` extension. One sidecar per namespace pays that once
  per start, not per query.
- **DuckDB is cheap to build and costly to ship; DataFusion is the opposite.** DuckDB's four files
  come to about 1.25× DataFusion's one, and the extension files are per platform and per DuckDB
  version. They fit a glibc-based distroless image (`distroless/cc`) but not `static`.
#### 2.8.5 Dialect and what it costs callers

`query_dialect` would report **`duckdb`** or something naming DataFusion. This is not a cosmetic
choice:

- **DuckDB has a documented dialect with a name.** Callers get a name they can look up, with
  DuckDB's own documentation behind it, and dolmen's README and `skill/` can point at it.
- **DataFusion's SQL is its own thing, modelled on ANSI SQL, with no registered family name.** A
  `query_dialect` value for it has to be either invented or borrowed, and either way it is a name
  dolmen owns and must keep meaning. The existing MySQL-ecosystem callers get no help from it.

**Object storage (S3) stays out of this spike** and is listed as untested, per the brief: DuckDB
needs `httpfs` (published and pre-placed, so the *mechanism* is available and unexercised) and
DataFusion would need `object_store` with an S3 store registered. Neither has been run against a
MinIO container here.


#### 2.8.6 Decision: DuckDB

Both engines confine, cancel and recover correctly through the same driver. Spilling under the lock
is not shown (§2.8.4). The choice comes down to three trades:

- **Speed: DuckDB.** 1.7–3.3× on scans, filters, `GROUP BY` and joins, and about 70× on a bare
  count; SQL over the object store is what a lakehouse is for. DataFusion starts faster.
- **Packaging: DataFusion.** One static file against four files and a per-platform extension set,
  but DuckDB builds in seconds while DataFusion takes 11–17 minutes per platform.
- **Hardening: DataFusion.** Rust, and a surface with nothing to switch on. DuckDB's lock held all
  24 attacks, but it is a set of settings whose order matters and which fail open silently when
  applied the wrong way (§2.8.3).

**The recommendation is DuckDB**, because speed is the property callers will notice and the other
two are costs dolmen pays once: in CI, and in a confinement test that `duckdb-lockdown` and this
battery already run on every change.

**Marc chose DuckDB on 2026-10-03.** The DataFusion sidecar and its CI jobs were removed from the
branch after the decision; the figures above are the record of what it measured.


---

## 3. Packaging: the DuckDB side when dolmen is one `CGO_ENABLED=0` binary

This is the question with the most options and the least obvious answer, so the reasoning is
recorded rather than the conclusion alone.

The constraint is hard and it is in CLAUDE.md as an invariant: pure Go, `CGO_ENABLED=0`, SQLite is
`modernc.org/sqlite`, embeddings are `rembed`, no cgo dependencies. The *binary* stays that way
under every option below — the question is only what else a deployment needs.

**Option A — a separate helper binary dolmen ships and releases.** dolmen spawns `duckdb` (or a
thin `dolmen-duckdb` wrapper dolmen builds) as a child. Cost: a second release artifact, six more
platforms in `make release` (`PLATFORMS` already lists six), and the user has two binaries to
install and must put the helper somewhere dolmen can find. Benefit: a pinned DuckDB version, one
artifact, no system dependency, and the container image gains a second file.

**Option B — a system DuckDB.** dolmen execs `duckdb` from `PATH` or a configured path. Cost: the
user installs and upgrades DuckDB themselves, so a dolmen release cannot pin the version and a
DuckDB upgrade can break `query` under them. Benefit: the smallest dolmen release, and it is how
every tool that shells out behaves.

**Option C — the container image carries it.** The `Dockerfile` currently builds one static binary
into `gcr.io/distroless/static`, which has no shell and no package manager. Adding DuckDB means
switching the runtime stage to something that has a shell and a package manager, or copying in a
statically linked DuckDB from a builder stage. The distroless pin is deliberate ("Pinned to the
OCI index digest for reproducibility"), so this is a real change to a deliberate choice.

**The plan recommends A, with B as a supported override, and C as a consequence of A** — subject to
the spike, which can dissolve this whole section. If §10 Q2's in-process pure-Go engine turns out to
confine `query` adequately, there is no helper binary, no second release artifact, no SBOM gap and
no distroless change, and the question answers itself. That is worth keeping in view while reading
the rest of this section: **everything below is the answer conditional on DuckDB working**, and the
cheapest possible outcome of the spike is that none of it is needed.

Given DuckDB does work, the reasoning for A: the `query` story is a **contract** decision (D28
disclosure, §0.5.3 confinement) and the plan is a **slices** decision, and the two must not be
coupled. Shipping the helper separately keeps the deployment story honest — an operator who wants
`query` installs a second binary, and one
who does not runs exactly the binary they run today — while letting the version be pinned and tested
in CI. It also keeps option B open, so the *deployment* choice is the operator's while the
*supported* configuration is ours.

**What this does to releases and `release.yml`.** Concretely, if A is taken:

- `Makefile`'s `release` target gains a second cross-compiled binary per platform. DuckDB's own
  distribution is not a Go cross-compile, so this is **not** `GOOS/GOARCH go build` — it is a
  per-platform download of the pinned DuckDB release, checksummed, staged into `dist/` alongside
  `dolmen-<version>-<os>-<arch>`. The checksums target already covers `dist/*` wholesale, so
  `SHA256SUMS` needs no change.
- `release.yml` needs the DuckDB version pinned in **one** place, and both the Makefile and the
  workflow read it — the same shape as `MODEL_RELEASE`, which already exists and already has a
  guard (the models workflow refuses a tag that does not match, and the release workflow refuses to
  build when no release is published under it). Reusing that pattern is the least surprising thing
  to do: the pin is a version constant, a mismatch is a build failure, and the failure text says
  what to do.
- The **SBOM** step (`anchore/sbom-action` over `.`) covers the Go module graph and would not see a
  downloaded DuckDB binary. That is a real gap in the release, and a release note saying so is not
  an adequate answer. Either the DuckDB binary gets its own SBOM, or the release body states
  plainly that the image and binaries carry a component the SBOM does not describe.
- The **GHCR image** gains the helper binary, which forces the `Dockerfile` runtime-stage change
  from `distroless/static`. The pin-to-digest discipline should be kept by pinning the new base the
  same way, and the SBOM gap applies to the image too.
- **Windows** is the awkward platform: it gets `duckdb.exe`, and a spawn-and-supervise path that
  works on both. The plan does not assume Unix-only process control; `os/exec` is portable, and the
  flags that a child needs (`memory_limit`) are DuckDB's own, not shell syntax.
- The **`-duckdb-path` flag** is what makes option B real: it defaults to the helper next to the
  dolmen binary and can point at a system `duckdb`. Without it, option A forecloses B, which is the
  thing that makes A acceptable to a local-first operator.

**The honest cost**, which is Marc's call and not the plan's: a deployment under option A is no
longer one file, and research-note open question 3 — *is a non-Go sidecar acceptable operationally
given the local-first ethos?* — is still formally open. The plan's position is that A is the least
bad shape of the answer, not that the question is settled. §10 Q5 puts it plainly.

---

## 4. The write path

### 4.1 Appends

`insert` is the format's native operation, so the work is not in the format but in three things
around it, each already specified in [storage-adapter-mechanics.md](storage-adapter-mechanics.md) §1:

**Row-id allocation.** The implicit `id` needs a server-assigned, monotonic, **never-reused**
allocator; Iceberg has no native equivalent. It is assigned inside the namespace-level
serialization point (below), never as `max(id)+1` from a snapshot — that collides under concurrent
writers and corrupts `read_rows`, change records, and idempotent replay. Adapter #1 gets this from
`INTEGER PRIMARY KEY AUTOINCREMENT` and `LastInsertId`
([internal/store/insert.go](../../internal/store/insert.go)); adapter #2 declares
`id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY`
([internal/postgres/table.go](../../internal/postgres/table.go)) and reads the id back with `RETURNING`.

The lakehouse has **neither mechanism**, which is the whole reason this is a problem. A Parquet
file has no id column that the format generates, and the id has to be a *dolmen* value written
into the row before it is committed. So the allocator is dolmen's own counter, allocated
transactionally in the namespace's SQLite file. Note this is a different answer from adapter #2's,
and deliberately so: adapter #2 could lean on an identity sequence because the database assigned
the id inside the same transaction that inserted the row. The lakehouse has no such assignment
point, so the allocator has to be one dolmen controls, and it is transactionally in the commit-log
write (§4.3) so an id and its change record cannot come apart.

The reserve-time hazard in §1.3 item 8 — a sequence allocating at reserve time, so two concurrent
writers can reserve change positions 1 and 2 and commit in the opposite order — is a hazard for
**change positions**, not row ids, and adapter #2 is explicit about the distinction
([postgresql.md](postgresql.md): "PostgreSQL identity sequences assign row IDs only"). It applies
unchanged to the change-sequence counter this lane also needs. Slice 6.

**Idempotency atomicity.** The idempotency record — key, payload hash, assigned ids — commits in
the **same** atomic serialization point as its rows, never as a separately committed metadata
table. A crash between two independent commits duplicates rows or replays phantom ids, which is a
§0.6 violation. This is a §0.6 atomicity obligation, not a nicety, and it is the reason the
serialization point below is a SQLite transaction at all.

**Serialization point.** §0.6 requires writes to one namespace to be observable in some serial
order, and §9.3 requires that the same point which orders commits assigns the change sequence. The
lakehouse's answer is the second option in
[storage-adapter-mechanics.md](storage-adapter-mechanics.md) §1: **a dolmen-owned namespace
commit log as the authoritative record**, with snapshots as idempotent materializations and
recovery by replay. Concretely, the point is a transaction on the namespace's SQLite file
(§4.3), and the Iceberg commit follows it.

### 4.2 Point deletes, and why they are position deletes

`update`/`delete`/`upsert`/`upsert_by_key` are position deletes — a Parquet position-delete file
committed through `RowDelta`. **Not deletion vectors.** The trial
([query-without-sql.md](query-without-sql.md)) proved that a DV write through iceberg-go v0.6.0
commits successfully and then leaves the table unreadable by the library's own scanner:
`not implemented: deletion vector read is not yet implemented`, with the cause visible in
`table/arrow_scanner.go` as a known gap. A DV-based adapter would be building on a feature that
cannot read its own output. D25 §0.5.1 and research §2.2 both answer the small-write problem with
DVs; on this evidence, that budgets for the wrong one of the two available answers.

The three API traps the trial recorded are carried into slice 8's test list, because each cost the
trial more time than the work it guards:

- `RowDelta.Commit` **does not commit** — it stages onto the transaction and `tx.Commit` is still
  required. Skipping it returns `nil` and leaves the table untouched: no snapshot, no error,
  nothing to notice.
- A **nil `CatalogIO` segfaults** on commit inside `doCommit` rather than returning an error.
- The snapshot summary's `added-data-files` is a **count, not a path**. DVs are keyed by data-file
  path and the only exported enumeration is `Scan().PlanFiles(ctx)`.

**The cost of the choice, stated.** Position deletes accumulate. Every point update appends a
delete file; reads apply them all. This is a compaction problem, not a correctness problem, and
slice 14 exists to bound it. It is the right trade while iceberg-go cannot read DVs — the
alternative is unreadable tables.

### 4.3 The namespace commit log

The change feed needs a per-namespace, gap-free order and Iceberg does not have one. Snapshots
order commits **within one table**; a namespace-wide feed spans tables, so a `(snapshot, position)`
cursor cannot provide the order, and the failure is the same reserve-vs-commit skip race as the
Postgres sequence hazard in §1.3: a client that observes and persists position 2 before position 1
commits has permanently skipped position 1, and `seq > ?` resume never returns it.

**The design: a dolmen-owned commit log in the namespace's own SQLite file, as the authoritative
commit record.** This is not a new file — it is the same SQLite file the namespace already has
conceptually for its catalog, opened through `modernc.org/sqlite` with no cgo, which the trial
proved works for `catalog/sql` with the modernc driver. The catalog lives there too, so commits need
no catalog service at all; that retires the research note's standing worry that a pure-Go Iceberg
writer would force a sidecar.

One transaction per mutation covers, atomically:

1. allocate the change sequence position (§0.6 ordering — this is the point that orders commits);
2. allocate row ids for the write;
3. write the idempotency record with its payload hash and assigned ids;
4. write the change records, each carrying the row's **owner** as internal authorization metadata
   (stamped engine-internally, not a format concept) and the table's **lifetime key**
   (`NsGen`, `Table`, `DropGen`);
5. commit.

Then the Iceberg snapshot commit follows as an **idempotent materialization**: replaying it is
safe, and recovery replays the log to converge. If the process dies between 5 and the snapshot
commit, the log says what should exist and recovery materializes it — which is the correct
direction for that crash, because the alternative is a change record for a commit that never
landed.

**Read visibility.** The log is the read-authoritative tail, not merely a recovery log: reads
(`read_rows`, `query`, the searches) overlay committed-but-unmaterialized log entries as a union
view, or the serving view advances synchronously before the write acks. Otherwise an acknowledged
write exists only in the log and §0.6 read-your-writes is violated until materialization. This is
the requirement that makes the log's authority a real commitment rather than bookkeeping.

**Cursors** are durable, opaque, and encrypted per §9.3: randomized authenticated encryption, a
fresh nonce per issuance, a fixed-length padded plaintext, under a persistent deployment-wide key.
The concrete precedent is `internal/postgres/changes.go`, which mints cursor tokens into a
persistent table rather than deriving them from positions, and adapter #1's
`mintCursorToken`/`resolveCursorToken`. A cursor is bound to the namespace lifetime that minted it
(it encodes `nsGen`), so a predecessor cursor replayed against a successor is an explicit
lifetime-mismatch error rather than a silent skip. Slice 10.

---

## 5. Search

### 5.1 Full text — native, not shared-Go

Per D27 (2026-09-19), which superseded the #315/#323 shared-Go BM25 decision: **each engine executes
its own full-text matching, ranking, and pagination.** No shared Go scorer, no cross-engine ranking
parity. [postgresql.md](postgresql.md) applied this to adapter #2 and the tree has no BM25 code.

So the lakehouse runs full text **below the seam, engine-side**, over the core expression grammar
that §7 pins on every engine: terms, implicit AND, `OR`, binary `NOT`, quoted phrases, `term*`
prefix, with §7's complete precedence ladder (juxtaposition, then `NOT`, then `AND`, then `OR`) and
bare leading `NOT` a syntax error. The extended grammar — `field:term`, `{a b}:term`, `NEAR(...)` —
is engine-documented and may be refused with a teaching error.

**The design.** The engine's own text index over the table's full-text fields, with the engine's
documented analysis (stemming, stop words, CJK) and ranking, computed in the engine, with `id`
ascending as the deterministic tiebreak. Result shape, filters/authorization, bounded pagination
and `truncated` are the contract and are shared. The conformance assertions are per-engine:
relevance is tested *within* this engine, never byte-compared to SQLite's FTS5 order. Marc
confirmed on 2026-09-29 that D27 settles this, so the "confirm D27" question the plan originally
carried is withdrawn.

**One interface note.** `store.Engine` has a `Tokenize` method
([internal/store/tokenize.go](../../internal/store/tokenize.go)), and the `tokenize` operation serves it:
it is how a caller asks "how does this server's engine analyse this text?". It is a contract op, so
the lakehouse must implement it, and its answer will be DuckDB's tokens, not SQLite's. That is the
same disclosure `query_dialect` makes, in a place the suite already reaches — and it is a good
reminder that the disclosure obligation is not one field.

**Where the ranking actually runs** is a §10 Q7 question, because it is not determined by the
contract: the engine could index text in the Iceberg/Parquet tier with a Go-side scorer, or lean on
the DuckDB sidecar (FTS, which is genuinely present there), or a third option. Each is defensible
and the choice has real cost differences, so it is Marc's.

### 5.2 Vectors

`search_vector` is a **natural fit** for this tier: a Parquet column scan, and the exact
brute-force path is the conformance reference. §7's canonical cosine is fully specified — both
operands normalized to float32, computed in binary64, component-wise in dimension order, every
multiply and add individually rounded, FMA and reassociation **forbidden**, zero norm scoring
exactly `0`, final quotient clamped to `[-1, 1]` — and it lives in
[internal/store/vector.go](../../internal/store/vector.go): `cosine`, the known-norm variant
`cosineKnown`, and the exported `store.Cosine` that adapter #2's
[internal/postgres/search.go](../../internal/postgres/search.go) already calls, which is how the two
engines get bit-identical exact results without sharing anything. (`internal/value` is **not** the
home of the scorer — it holds vector *input* coercion and typed decoding, extracted for adapter #2
alongside the rest of the value layer.) `q(s) = floor(s / fl64(1e-9))` then gives the
order, with `id` ascending as tiebreak, and `skipped_vectors` counts corrupt,
dimension-mismatched, and non-finite stored vectors rather than dropping them silently.

**Engine 3 declares `VectorExecution: VectorExact` and `ANNRecallBound: nil`.** D26's accelerator
exception is available — a lakehouse is the tier where an ANN index would be most tempting — but
nothing in the contract requires it, the deterministic K-independent sequence is the hard part, and
declaring it is a commitment this lane has no reason to make in v1. Recording the choice here
because "we did not take the accelerator" is a decision, and a later one that takes it will need
the per-response `"execution"` field, a declared recall bound, and prefiltering over the visible
corpus.

Under `auth: on` a `RowScope` filters the corpus **before** ranking, never after: foreign vectors
must not consume the candidate budget or reorder the caller's results. On the exact path that is
a filter; on an ANN path it is a prefilter requirement. Either way the lakehouse's append-dominated
tier and `row_access` rarely coincide, so the practical case is small — but the contract is
unconditional, so the slice carries the test.

---

## 6. Conformance: what runs, and from which slice

The suite is already engine-parameterized: `DOLMEN_ENGINE` selects the engine for the whole
package, `internal/conformance/engine_test.go` resolves it, and CI's `postgres-foundation` job runs
`./internal/conformance`, `./internal/postgres` and `./internal/blackbox` against a PostgreSQL 17
service. The lakehouse rides the same rails. **The rule from research §1.4 item 2 holds: engine
policy is applied at test/subtest granularity, never by whole-file tags**, because a whole-file tag
silently drops the engine-neutral coverage in that file and lets a lakehouse regression in those
paths escape.

| slice | what runs against the lakehouse |
|---|---|
| 3 | Nothing opens the engine. The proof is that SQLite and Postgres are unchanged: the existing matrix green, and `TestEngineKnobResolution` extended with the third name. |
| 4 | The **namespace/lifecycle** subset — `TestNamespaceBackendConformance`-shaped, both engines in one test as `internal/conformance/postgres_namespace_test.go` does. Not the full suite. |
| 5 | **Table lifecycle** added to the same dual-backend pattern. |
| 6–8 | **Write and read** conformance: insert, idempotency, mutation, typed reads. Slice 8 adds the update/delete/upsert subset. |
| 9 | **Search**, with per-engine relevance expectations. |
| 10 | **Change feed** — `changes_since` and `wait_for` — including the gap-free, cursor-durability, and lifetime-mismatch properties, which are the ones a snapshot-diff design would fail. |
| 11 | **`query`**, with `query_dialect` compared rather than assumed, and the confinement tests the §2.5 spike already wrote. |
| 12 | **The full suite**, plus `./internal/blackbox` driven against the real binary with `-engine lakehouse`. |
| 13 | `subscribe` only if it is implemented. |

**What skips, and why.** The same categories adapter #2 skips: probes of storage internals, and the
extended-FTS syntax this engine refuses. Those probes are the out-of-band writes that reach past the
API and operate on the storage file directly — the FTS5 stemming surgery in `search_test.go`, the
blob and storage-class surgery in `limits_test.go` and `waitfor_test.go`, all of which go through
`h.outOfBand`; and separately the `CAST(... AS BLOB)` alias semantics in `coercion_test.go`, which
needs no out-of-band write and skips for being a SQLite type-affinity behaviour. (The helper those
writes go through, `outofband_test.go`, contains only the `openSQL` wrapper and no tests of its
own; it is the *callers* that are SQLite-only.) The **contract-facing fidelity assertions keep
running** — negative-zero normalization, int64 endpoints, exponent-format bands — because they are
the validation of the normalization layer, and skipping them would leave the contract unpinned on
this engine. Error-message pins fork **by input, not by assertion**: subtests whose SQL is valid on
both engines keep running, which is the coverage proving the dialect's errors normalize to the same
teaching strings.

**CI.** Slice 4 adds a `lakehouse-foundation` job on Linux, macOS and Windows, running the
namespace/lifecycle subset and catalog tests with the race detector. Filesystem fixtures always
run in private temporary directories; no DSN, sidecar or optional skip is needed for this slice.
The full lakehouse suite remains refused. From slice 11, the DuckDB child is provided as
a service or a downloaded pinned binary, `DOLMEN_TEST_PG_DSN`-shaped knobs
(`DOLMEN_TEST_LAKEHOUSE_*`), and — following the Postgres precedent — the job **fails rather than
silently skipping** when its DSN is missing. A suite that quietly skips on a new engine is a suite
that reports green without having run.

**Fixtures at test/subtest granularity** means the sidecar CI job grows a
`DOLMEN_TEST_LAKEHOUSE_REQUIRED=1` and the skip helpers gain a name per engine. Slice 3 is where
the helpers are fixed; slices 4–12 add the tags.

---

## 7. The dependency pins, and keeping them from breaking

### The pins

| module | pin | why |
|---|---|---|
| `github.com/apache/iceberg-go` | v0.6.0 | The version the trial proved: Parquet write/read, table creation at v2 and v3, append from Arrow, position deletes through `RowDelta`, `catalog/sql` over SQLite. |
| `github.com/apache/arrow-go/v18` | **v18.6.0** | iceberg-go v0.6.0 does not compile against v18.8.0: that requires `twmb/avro v1.8.0`, whose `SchemaNode.Root()` signature change breaks iceberg-go's `internal` package in three places. A clean resolve picks the broken combination, so the consumer must hold Arrow back. |
| `github.com/parquet-go/parquet-go` | v0.32.0 (or whatever the resolve lands on) | The Parquet reader/writer the trial used. |
| `modernc.org/sqlite` | already v1.57.0 | The catalog and commit log. `catalog/sql` takes a `*sql.DB`; bun's dialects are descriptors, not drivers, so the cgo `sqliteshim` is never imported and nothing pulls in cgo. |

**Size is a real cost but not the objection the research note feared.** dolmen's static binary is
49.1 MB; adding `iceberg-go/table`, `catalog/sql`, Arrow and `parquet-go` takes it to 68.0 MB —
**+19 MB, +39%** — and it stays a `CGO_ENABLED=0` pure-Go static binary with no `runtime/cgo` in
the dependency graph. For scale, Parquet alone is 18.7 MB standalone and the whole Iceberg stack
is 70.9 MB. The single static binary survives; it just gets bigger, and a per-engine subpackage is
the mitigation if that matters (see below).

### Keeping them from breaking

A pin with no enforcement is a comment, and the failure mode is the worst kind: an unrelated
`go get -u` resolves a newer Arrow and the build breaks three files inside a dependency, with an
error message that names none of dolmen's code. Five mechanisms, in order of how much they cost:

1. **A version-pin test** (lands with slice 4, the first code that imports the modules). A test that
   fails when the resolved `arrow-go` version is not exactly the pinned one, with the reason in the
   failure text. This is the one that matters: it turns a cryptic compile error into a named pin,
   at the moment of the change that broke it.
2. **`govulncheck` in CI** already runs (`make vulncheck`, and the release job's vulnerability
   gate). A pin held back for compile-compatibility is exactly the kind of thing a vulnerability
   scanner will eventually flag, and when it does, the failure text should name the pin. Worth
   writing down now.
3. **A `CGO_ENABLED=0` build in CI.** **Landed 2026-09-29** as the `cgo-free-build` job in
   [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml), outside the lane, because the gap
   was pre-existing and not the lakehouse's to leave: the `test` job runs `CGO_ENABLED=1 go test
   -race ./...`, so the only cgo-free builds were `make build`, `make release` and the
   `Dockerfile`, all at release time. The job runs `CGO_ENABLED=0 go build ./...` and
   `CGO_ENABLED=0 go vet ./...`, and both halves are load-bearing — checked rather than
   assumed: the build fails only when a cgo package is in the import graph and passes when it sits
   unimported, while `vet` fails in both cases. The temptation this closes is exactly "add a cgo
   driver to reach DuckDB for the query path", which the lane's own decision note rules out.
4. **Keep the dependency off everyone's build.** Adapter #2 learned this the hard way: importing
   `internal/postgres` from the root package put pgx and the embedded WASM PostgreSQL parser into
   every consumer's import graph, and the external-module example grew from 13.1 MB to 35.3 MB
   whether or not it used PostgreSQL. The fix was a first-party subpackage (`postgres.With`), so
   only programs that import it link the driver. **The lakehouse must do the same**: `internal/lakehouse`
   holds the engine, `lakehouse/` holds the facade option, and the root package must not import
   either. At +19 MB the argument is stronger here, not weaker, and this is a design constraint
   imposed by the packaging answer in §3, not an afterthought.
5. **A watched upstream, not a guessed upgrade.** iceberg-go v0.6.0's DV read gap is a known
   upstream gap (`table/arrow_scanner.go` says its per-file delete maps change "when
   `readAllDeletionVectors` lands"). When it lands, the Arrow pin and the position-delete choice
   are both re-examinable together. That is a review trigger, not a routine bump — which is the
   right relationship with a pin this load-bearing.

### One consequence worth stating

Because the facade's engine selection is `WithEngine("lakehouse")` refused unless the
`lakehouse` subpackage is imported (adapter #2's rule, and the lesson from its 13.1→35.3 MB
regression), **a program using the lakehouse does not need DuckDB at build time** — it needs the
helper at run time. That is the mechanical reason options A and B in §3 are both viable, and it
is why the +19 MB lands only on the programs that ask for the engine.

---

## 8. What this lane does not do

Recorded so a reader does not have to infer it, matching the spec's own "deliberate non-features":

- **High-frequency OLTP.** D25 and §0.5.1 put tens of thousands of point mutations per second out of
  scope for this tier. That is the operational store's job.
- **Serving/cache tier as a first-class component.** Research §2 open question 1 — analytics
  property or dolmen-side durability — was never answered, and it decides whether the serving tier
  is a v1 or a v3 concern. §10 Q1.
- **Data portability.** No `export`/`import` (D20, deliberately skipped). Open-format storage means
  Spark/Trino/DuckDB can read the Parquet — a property of the storage, not a dolmen op.
- **`export`-shaped migration tooling, change streaming, webhooks.** Out of lane.
- **A spec amendment, unless the spike says one is needed.** Every slice here is written to fit the
  spec as it stands. Slice 11 is the load-bearing one: it **cannot** start unless D28 and §0.5.3
  are satisfied by construction, and it proves it with the confinement tests rather than asserting
  it. If the spike finds DuckDB cannot confine itself, the reserved amendment becomes the path and
  it lands spec-first per the repo's own deviation rule — with a named availability class beside
  `subscribe`, a reworded §0.5.3 binding only to engines that have SQL, and a conformance corpus
  that skips `query` by declared capability rather than by engine name.

---

## 9. Slices needing more than one PR, and the ones at risk

So the order is not read as false precision:

- **Slice 8** is the largest single slice. `update`/`delete`/`upsert`/`upsert_by_key` on position
  deletes plus the read-side merge of deletes is plausibly two PRs (delete path; update/upsert
  path). It is listed as one because the position-delete machinery is shared; if it overruns, split
  it and say so rather than shipping half a delete path.
- **Slice 10** is comparable. The commit log and its cursors are one mechanism, but the
  `RowScope`/lifetime filtering is a second. The research note classes change-feed mapping as M and
  the Postgres adapter took several slices for the same surface.
- **Slice 11** is the one with genuine design risk, and it is the risk the trial did **not** retire.
  The trial proved the pure-Go tier; it said nothing about whether a DuckDB process can be confined
  strongly enough that §0.5.3 holds by mechanism (§2.4). **This is now answered by running the
  question rather than planning around it** (Marc, 2026-09-29): the lockdown spike in §2.5 moves
  ahead of every engine slice and decides it. If DuckDB cannot confine itself, §10 Q2's chain
  applies — an in-process pure-Go engine first (`go-mysql-server`), and if that cannot confine
  either, `query` ships declared unavailable through the reserved spec amendment. The plan's own
  original middle option, a narrower validated `query`, is **withdrawn**: Marc ruled that a
  statement filter is never the answer, and D28's disclosure-only obligation is not something a
  lane narrows on its own.
- **Slices 4–7** are individually unremarkable and collectively the bulk of the line count, and
  they are the slices with the **weakest** budgeting evidence. Research §2.3 gives effort classes for
  the append write path (M), update/delete (M), change-feed mapping (M), the FTS sidecar (M) and
  the read/serving tier (L) — it does **not** class the table-DDL work, which §2.2 lists only as a
  "natural fit" where "Iceberg schema evolution is a strength". So slices 4 and 5 carry no M/L
  estimate from the note at all, and slices 6–8 inherit one written before the position-delete
  decision and before this plan's commit-log design. §2.3's aggregate for the whole lane is
  "quarters, not weeks", and that estimate is for a lane **without** a SQL sidecar; this plan adds
  one. Treat the order as sound and the per-slice sizing as not yet known.

---

## 10. Questions for Marc

**Answered 2026-09-29.** Marc's decisions are below, each with its date and the section it changes.
The ones still open are marked, and each carries the assumption the plan is running on so the
question can be answered later without unwinding work.

### Answered

2. **How far does `query` go if DuckDB cannot be confined by its own settings?** **Answered by the
   spike (2026-09-29): it can, so the fallback chain is not triggered.** DuckDB v1.5.6 confines
   itself to one namespace's data directory completely, including against `ATTACH`, the sibling
   namespace, the namespace's own SQLite catalog, a symlink planted inside the allowed directory,
   `..` traversal, `COPY` in and out, `COPY ... TO PROGRAM`, the file readers, extension install and
   load, http(s) URLs, and every attempt to re-open the settings from a session. Slice 11 proceeds
   as pass-through SQL. The chain below stands for the record, and for the in-process question it
   opens rather than closes:
   1. **An in-process pure-Go SQL engine** — DoltHub's `go-mysql-server` is the first candidate.
      The decision note never evaluated an in-process engine, because at the time the only shapes
      on the table were embedded cgo DuckDB (ruled out) and an external process. The spike does not
      need it, but it has one advantage the sidecar does not: no second binary, no second release
      artifact, no SBOM gap, and no socket.
   2. **If a confined engine cannot be had at all, the lakehouse ships with `query` declared
      unavailable**, through the spec amendment
      [query-without-sql.md](query-without-sql.md) reserved: a named availability class beside
      `subscribe`, a reworded §0.5.3 binding only to engines with SQL, and a conformance corpus
      that skips `query` by declared capability rather than by engine name.
   3. **Never a statement filter.** A filter dolmen applies is not §0.5.3's mechanism, and Q4 below
      rules it out independently.

   This supersedes the plan's original preference for a narrowed `query` surface, which was the
   middle option here and is now gone: the choice is a confined engine, or the amendment, or
   nothing.

4. **Is `ATTACH` reachable from caller SQL, and if so what stops it?** **DuckDB's own settings are
   the only guard, and the spike proves it works** — no statement filter in dolmen, not even as a
   second layer. §2.4 carries the settings with the measured result for each, and one correction
   worth repeating because it inverts the plan's original reading: `enable_external_access=false`
   is the guard, and `allowed_directories` *widens* what stays reachable rather than narrowing it.
   Set the list alone and there is no sandbox at all.

5. **Is a second released binary acceptable?** (research §2 open question 3, unanswered since
   2026-09-14.) **Answered by the two spikes on 2026-09-29: the in-process option is off the table,
   so the cost is §3's and Marc's to accept.** See §2.6 for the side-by-side and what it settles.
   The short form: the in-process engine is pure Go but does not confine, DuckDB confines but needs
   a second artifact, and the trade is no longer "which engine" — it is "a helper binary, yes or no".

6. **Shared-Go BM25, or native?** **D27 settles it** — native, per-engine ranking, as
   [postgresql.md](postgresql.md) already does for adapter #2. No spec amendment. Slice 9 is
   planned this way and the plan's original "Q6 asks for confirmation" is withdrawn.

12. **Should a `CGO_ENABLED=0` build gate land in CI?** **Yes, now, in its own PR outside the
    lane** — landed 2026-09-29 as the `cgo-free-build` job in `.github/workflows/ci.yml`, running
    `CGO_ENABLED=0 go build ./...` and `CGO_ENABLED=0 go vet ./...`. Separate so it is not
    entangled with engine work: the cgo-free invariant is a property of the repo, not of this lane.
    Both halves are load-bearing, which was checked rather than assumed — the build fails only when
    a cgo package is in the import graph and passes when one sits unimported, while `vet` fails in
    both cases.

### Decided, and recorded as facts of the order rather than questions

- **The order.** Marc approved **4 → 5 → 6 → 11, then 7–10, 12–14 on 2026-10-03**, after
  spike 3 merged and DuckDB was chosen. Slice 11 needs the real tables and data from slices 5
  and 6. The `go.mod` pins (was slice 2) land **with slice 4** and their first imports. §1 carries
  the revised table.
- **The pins as a slice are withdrawn** rather than reordered, because a pin with no importer is a
  pin nothing exercises.

### Answered since the last revision

- **Q5's shape, settled by Marc on 2026-10-02.** The lakehouse **ships `query`** — SQL over the
  object store is the point of a lakehouse, so "ship without query" is off the table. It runs in a
  **sidecar process**: a second released binary that dolmen talks to over a pipe, with dolmen itself
  staying `CGO_ENABLED=0`. **Linking an engine in with cgo is rejected**, because an engine crash or
  a memory-safety bug would take down or expose the whole server. This closes the earlier framing of
  Q5, which asked whether to accept a second released binary at all; that is now settled, and the
  remaining question is narrowed to **which engine the sidecar runs**. §2.8 is the comparison, and
  **Marc chose DuckDB on 2026-10-03** (§2.8.6).
- **The DuckDB fallback is not a source build.** §2.7 recorded one; §2.8.1 removes it. Prebuilt
  `libduckdb` covers every target platform, the `-musl` zips ship a static `libduckdb_static.a` for
  both linux architectures, and `iceberg`/`httpfs` are published prebuilt for all five platform
  tokens and load from disk with no network. **What this changes:** §3's cost for the DuckDB side is
  one small `g++` invocation and two downloaded artifacts, not a C++ toolchain in the release
  pipeline and a 40-minute DuckDB build per platform.

### Still open, pending the spike

Each carries the assumption the plan runs on meanwhile.

1. **Analytics property, or dolmen-side durability and scale?** (research §2 open question 1,
   unanswered since 2026-09-14.) Decides whether the serving/cache tier is a v1 component or a v3
   one, and so whether slice 6 appends to Parquet or through a serving tier. **Assumption:
   durability first** — the serving view is the log-backed union view of §4.3, and a real serving
   tier is later.

3. **The error code for a sidecar that is down?** `internal/derr`'s ten codes have no fit;
   `embedder_unavailable` is the right *shape* but the wrong *subsystem*, and reusing it would
   tell a caller to configure an embedder. **Assumption:** a new code beside
   `embedder_unavailable` (`sql_engine_unavailable` is the name it would take), with `query_error`
   reserved for SQL the engine itself rejected. Note this becomes moot if Q2's amendment path is
   taken, since there is no sidecar to be down.

7. **Where does full-text ranking run?** D27 settles that it is not shared, not *where*.
   **Assumption:** Go-side in the Iceberg tier, so full text does not depend on a sidecar being
   available. That assumption becomes load-bearing if Q2 goes the in-process route.

9. **Should `subscribe` be attempted?** §9.3 permits only `subscribe` to be declared unavailable,
   and never `wait_for`. **Assumption:** attempted in slice 13, with declaring it unavailable an
   honest, permitted option if it cannot be.

10. **The row-id and change-sequence high-water mark after a catalog rebuild.** A rebuild from
    Parquet re-derives the data but not the allocator. **Assumption:** the namespace's SQLite file
    is the authority for both, and losing it loses the counters — a real limitation for a namespace
    meant to be recovered from Parquet alone, and it shapes what `restore` would mean here.

11. **Should deployment topology become a capability field?** §0.6 calls it engine-declared and no
    engine has anywhere to declare it: `EngineCapabilities` has six fields, none of them topology,
    and both adapter #1 and #2 declare it in prose only. **Assumption:** prose, as the two existing
    engines do. Adding the field is contract-surface work (op schema, OpenAPI, MCP, conformance)
    with no precedent in the tree.
