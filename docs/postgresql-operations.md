# Running dolmen on PostgreSQL

SQLite is the default engine and needs no setup. PostgreSQL is opt-in: it is the
engine to pick when several dolmen processes must serve one dataset, when the data
outgrows a single file per namespace, or when an existing PostgreSQL deployment
already owns backup, failover, and access control.

This is the operator's guide. The design record — what each increment implemented and
why — is [docs/design/postgresql.md](design/postgresql.md).

## Requirements

- PostgreSQL 16 or newer. The storage features dolmen uses are older than that —
  generated `STORED` columns arrived in 12, `pg_notify` and column-level `GRANT` long
  before — but the per-grant `INHERIT FALSE, SET TRUE` options below are new in 16. On 15
  and earlier, inheritance is an attribute of the member role rather than of the grant,
  so keeping the backend from implicitly holding the query role's grants means making the
  backend `NOINHERIT` for every membership it has. CI runs against 17.
- No extensions. Full-text search uses the built-in `tsvector` machinery and vector
  search is executed in dolmen, so no `pgvector`, no `pg_trgm`, no superuser step.
- No `CREATEROLE` at runtime. Dolmen never creates a role; it validates the roles the
  deployment provisioned and refuses to start serving caller SQL if they are wrong.

The driver is pure Go, so a PostgreSQL deployment does not change how dolmen is built:
`CGO_ENABLED=0` still produces one static binary.

## Provisioning the database

Dolmen needs one database and two roles. The backend role is what dolmen connects as.
The query role is what caller-supplied SQL runs as, and it exists so that a `query`
call cannot reach anything the caller was not granted.

```sql
CREATE DATABASE dolmen;

CREATE ROLE dolmen_backend LOGIN PASSWORD 'replace-me';
GRANT CONNECT, CREATE ON DATABASE dolmen TO dolmen_backend;

CREATE ROLE dolmen_query
  NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT dolmen_query TO dolmen_backend WITH INHERIT FALSE, SET TRUE;
```

`SET TRUE` is the half that does the containing. It lets the backend switch into the
query role for the duration of one caller statement, and `SET ROLE` *replaces* the
effective privilege set rather than adding to it, so caller SQL runs with the query
role's privileges and nothing else — the backend's `CREATE` is simply not there. Dolmen
requires this membership and refuses to serve caller SQL without it.

`INHERIT FALSE` is the half that keeps the two privilege sets from merging: without it
the backend implicitly holds every grant made to the query role. That is hygiene rather
than containment, and it matters most as the query role accumulates grants over time.

Note what this does *not* do, because it is easy to read the grant backwards. Membership
flows one way: a member may hold the privileges of the role granted to it, never the
reverse. No grant option can make the query role carry the backend's privileges. The
escalation to avoid is therefore the grant written the other way round —
`GRANT dolmen_backend TO dolmen_query` — and any other membership handed to the query
role, since `SET ROLE` lands caller SQL in whatever that role can reach. Keep the query
role's memberships empty.

`CREATE` on the database is required because dolmen creates one schema per namespace.
It is not required to be a superuser and should not be one.

### What dolmen checks before serving caller SQL

The check is not at boot. The first `query` call against a namespace verifies the query
role before running anything, and refuses with a message naming the problem — so a
mis-provisioned role surfaces on the first caller query, not at startup:

- The role must exist.
- It must be `NOLOGIN`, `NOINHERIT`, `NOSUPERUSER`, `NOCREATEDB`, `NOCREATEROLE`,
  `NOREPLICATION`, and `NOBYPASSRLS`. Any one of these being wrong is a refusal, not a
  warning — a login-capable query role is a credential, and an inheriting one is an
  escalation.
- The backend account must hold `SET` membership in it.

Omitting the query role entirely is supported. Every other operation works; `query`
alone refuses, saying a pre-provisioned role is required. Deployments that do not
expose caller SQL can leave it unset and skip the second role.

### What the query role can read

Dolmen grants the query role `USAGE` on each namespace schema and column-level `SELECT`
on exactly the declared fields plus `id` and `created_at`. It is never granted the
table as a whole. Two consequences worth knowing, because both are deliberate and both
are pinned by tests:

- The stored embedding column (`_embedding`) and the generated full-text column
  (`_fts`) are not readable from caller SQL. They are storage, not schema.
- A column added by a later `migrate` is readable immediately: the migration re-grants
  the table in the same transaction that alters it, so there is no window where the
  schema advertises a column that caller SQL cannot select.

## Selecting the engine

From the binary:

```sh
dolmen -engine postgres \
  -pg-dsn 'postgres://dolmen_backend@db.internal:5432/dolmen?sslmode=verify-full' \
  -pg-catalog dolmen_catalog \
  -pg-query-role dolmen_query
```

`DOLMEN_PG_DSN`, `DOLMEN_PG_CATALOG`, and `DOLMEN_PG_QUERY_ROLE` are the environment
equivalents. Prefer the environment for the DSN: flags are visible in process listings
and a DSN usually carries a password.

`-engine postgres` without a DSN is refused, and a DSN without `-engine postgres` is
refused too, so a half-configured server fails at startup instead of quietly serving
SQLite from the local disk.

`-data` still matters. No table data lands there, but the grant registry and the local
embedding model cache do, so keep it on persistent storage if you use `-auth on` or the
local embedding provider.

From the Go library:

```go
import (
    "github.com/lsm/dolmen"
    "github.com/lsm/dolmen/postgres"
)

st, err := dolmen.Open("", postgres.With(postgres.Config{
    DSN:       os.Getenv("DOLMEN_PG_DSN"),
    Catalog:   "dolmen_catalog",
    QueryRole: "dolmen_query",
}))
```

The driver lives in its own package so that programs which do not use PostgreSQL do not
link it. `dolmen.WithEngine("postgres")` on its own is refused with an error naming the
import to add. The data directory argument is SQLite's, so pass `""`.

One live store per catalog per process is enforced. The identity is the connection's
host, port, database, and user as pgx parses them, plus the catalog name — not the DSN
text — so two spellings of the same target collide instead of opening two pools over one
catalog.

## Catalog layout

`-pg-catalog` names one schema holding dolmen's own bookkeeping: the catalog version,
the namespace and table registries, the change log, cursors, the migration log, and
idempotency keys. Every namespace gets its own separate schema for its tables.

Two dolmen deployments can share one database by giving each its own catalog name. They
will not see each other's namespaces, and each one's `LISTEN` channel is derived from
its catalog name, so notifications do not cross.

The catalog is created on first connect under an advisory lock, so several processes
starting at once is safe. It is also upgraded in place on connect: a newer binary adds
what it needs and bumps the stored version. There is no separate migration command and
no migration to run by hand.

Rolling back to an older binary is the case that does not work. A binary refuses to
start against a catalog whose version is newer than it understands, with a message
saying to use a compatible release, rather than operating on a layout it may not
interpret correctly. Upgrade all processes on a shared catalog together, or stage the
upgrade with a second catalog.

## Operating notes

**Connection pool.** The pool defaults to pgx's own sizing. Set `MaxConns` on the Go
config to bound it. Size it against the database's `max_connections` budget across every
dolmen process, not per process.

**Subscriptions.** `subscribe`, `wait_for`, and `changes_since` are served from the
durable change log, so they work across processes: a writer on one node wakes a
subscriber on another through `pg_notify`. A subscriber that falls far enough behind is
closed with a teaching error carrying a resume cursor, rather than being allowed to grow
without bound.

**Change retention.** `-change-retention` (default 168h) bounds the change log. Pruning
happens when the change feed is read, not when rows are written, so a namespace that is
written to but never subscribed to or polled keeps its change log until something reads
it. `0` disables pruning entirely, which means the change log and its cursors grow
forever — deliberate for audit use, but then the log is yours to manage.

**Statement timeout.** Caller SQL runs with `statement_timeout` set to 30s for the
statement. A query that exceeds it comes back as a timeout naming the remedy, not as a
stuck connection.

**Backups.** The dataset is ordinary PostgreSQL: `pg_dump` of the database captures the
catalog schema and every namespace schema consistently. Dumping a single namespace
schema without its catalog produces something dolmen cannot read.

**Monitoring.** Dolmen sets `application_name` to `dolmen`, so `pg_stat_activity` can be
filtered to it.

## Moving an existing dataset

There is no built-in SQLite-to-PostgreSQL copy. Moving a dataset means replaying it
through the API: create the namespaces and tables on the PostgreSQL deployment, then
read rows out of the SQLite deployment and insert them. Table schemas transfer exactly,
since both engines store the same schema registry.

Two things do not transfer, and both matter more than they look:

- **Row ids are reassigned.** Anything outside dolmen holding a row id needs remapping.
- **Cursors do not carry over.** Change-feed cursors are engine-local. Subscribers
  resume from `begin` or from the new head, not from a cursor minted against SQLite.

## Behavior differences from SQLite

Both engines pass the same conformance suite, which is what "parity" means here — it is
enforced by tests, not by shared code. The deliberate exceptions are all in full-text
search, because each engine uses its own native implementation and ranking rather than a
shared scorer:

- SQLite uses FTS5 with BM25. PostgreSQL uses `tsvector` with `ts_rank_cd` and the
  `english` text-search configuration.
- Stemming, stop-words, and accent handling therefore differ. `running` and `run` match
  each other under both, but not always identically, and a term that is a stop-word in
  one may not be in the other.
- Result *ordering* within a set of matches can differ. Which rows match is far more
  stable than what order they come back in.

Ranking is not portable between engines, so do not pin a test to an exact score or to a
tie-break order across a migration.
