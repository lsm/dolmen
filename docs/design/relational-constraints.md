# Foreign keys and partial unique indexes

**Proposed for #700; implementation (#701) waits for human approval.** Add portable,
managed constraints to typed tables, preserving the engine consistency and visibility
contracts in [identity-and-engines.md](identity-and-engines.md). Every row keeps its
single system `id`; composite primary keys and arbitrary SQL DDL remain out of scope.

## Schema and API

Add `constraints` to create/describe schema and named `add_constraint` /
`drop_constraint` migration changes. Names follow field-name rules and are unique
within a table; allow at most 32 constraints per table. Proposed wire declarations:

```json
{"name":"active_slug","kind":"unique","fields":["slug"],"where":"retired_at IS NULL"}
{"name":"project_ref","kind":"foreign_key","field":"project_id","references":{"table":"projects","field":"id"}}
```

`where` is optional (omitted means unconditional). The Go façade gains matching
`Constraint`, `UniqueConstraint`, `ForeignKeyConstraint`, and `CreateTableOptions`
values through additive `CreateTableWithOptions`; existing `CreateTable` is unchanged.
`TableSchema.Constraints` and migration descriptions round-trip the declarations on
Go, HTTP and MCP. Parent tables must opt in with `accept_references: true`
(`CreateTableOptions.AcceptReferences`, default false), also reported by describe.
Under `auth: on`, enabling at creation or changing this policy requires `schema`
and namespace-wide `read`, even when no references exist. Changes use
`set_accept_references` with `expected_version`; disabling fails while references
remain. This is a managed invariant, not an `UpsertByKey` matching hint.

## Meaning

- **Unique:** one to eight distinct declared scalar fields (`string`, `text`,
  `number`, `boolean`, `timestamp`); composite secondary uniqueness is supported.
  Rows participate only when `where` evaluates TRUE; FALSE/NULL do not participate.
  If any key component is NULL, duplicates are allowed. Compare coerced values:
  bytewise case-sensitive text/timestamp strings and exact numeric equality
  (1 equals 1.0; comparing integers must not round them through float64). Reject
  non-finite constrained numbers. Timestamp equality and predicate ordering compare
  the strings returned by `CanonicalTimestamp`, not instants: it preserves offsets,
  fractional precision and accepted date-only forms. Thus `2026-10-23T12:00:00Z`
  and `2026-10-23T08:00:00-04:00` are distinct keys. Timestamp predicate literals
  pass the same coercion before bytewise comparison; do not cast to a backend
  temporal type. JSON, vector, secret and file keys are excluded.
- **Predicate:** a bounded subset of the shared filter AST: scalar column/literal
  comparisons, `IS [NOT] NULL`, parentheses and AND/OR/NOT. No parameters, functions,
  SQL, subqueries, hidden columns or other tables. Predicate fields use the same
  scalar types as keys. Existing filter length/depth limits apply. Parse once and
  evaluate with shared typed semantics; never interpret backend-specific SQL.
- **Foreign key:** one declared `number` field references another table's system
  `id` in the same namespace, including a self-reference; the parent must have
  `accept_references: true`. Non-NULL values must be
  exact positive int64 IDs; NULL is allowed unless the field is required. No
  natural-key targets, composite references, cross-namespace links or cascade actions
  in this increment. Delete is RESTRICT; system IDs cannot be updated.
- **Timing:** enforce after each single operation, or each write inside `Batch`,
  against that write's complete resulting state. Constraints are immediate, not
  deferred: insert parents before children and delete children before parents.
  Multi-row writes are checked as a whole, including duplicates within the input.
  Any violation rolls back the entire operation/batch, including feed and idempotency
  records. Idempotent replay returns the saved result without reapplying the write.

## Ownership and permission boundaries

On `row_access: own` tables, uniqueness includes the row's stored owner implicitly,
regardless of the caller's current grants. Foreign keys require both endpoints to
be plain tables or both to be own-row tables; mixed declarations are refused. On
own-row tables, a child may reference only a parent with the same stored owner.
An absent parent and another owner's parent produce the same failure. A NULL owner
is a distinct unowned domain, never an empty principal string: NULL matches NULL for
owner-domain equality, so duplicate non-NULL unique keys conflict within that domain.
A NULL-owner child can reference only a NULL-owner parent, even under `auth: off`;
a principal-owned child can reference only that principal's parent. Auth-mode flips
do not relabel rows or relax these rules. NULL-owner children therefore cannot block
a principal-owned parent's deletion; they do block their unowned parent under
RESTRICT. Adding constraints rejects legacy rows violating these rules atomically.

Under `auth: on`, authorization precedes existence and data checks. Every `delete`, `drop_table` and `migrate set_row_access`
on a table with `accept_references: true` requires namespace-wide `read`, whether
or not it has incoming references, matching rows, or a foreign-key violation. Check
this stable, caller-targeted schema policy before looking up referencing tables.
A caller lacking that grant gets the same `forbidden` outcome with zero, readable
or hidden incoming tables. Do not condition this gate on a reverse-reference count;
removing the last FK does not remove the gate. Non-opted-in tables cannot acquire
incoming FKs and retain existing delete permissions. The opt-in deliberately
reserves parent deletion to namespace readers, including on own-row tables.

Plain-table uniqueness/outgoing-FK checks additionally require table-wide `read` on
each consulted endpoint. Own-row checks require a standing own-row read on each
endpoint (`read` or a data verb); a table-wide mutation requires read of all affected
domains, including unowned rows. Constraint schema changes require `schema` and
`read` on all affected tables. These amend only the stated constrained/opted-in
operations. No error exposes conflicting values, row IDs, owner labels, hidden
names or counts; neither its message nor outcome may reveal hidden references.

## Atomic enforcement and lifecycle

Persist declarations in the schema registry and enforce every managed write path:
insert, update, both upserts, delete, Batch, migration and future import. Hold the
namespace serialization boundary while checking candidate state and committing.
SQLite/PostgreSQL can use native indexes/checks where semantics match; they are
accelerators, not a substitute for the portable rules. Lakehouse checks the
read-authoritative state plus staged writes before publishing its namespace commit;
it must not depend on a SQL sidecar. Start with correct scans where necessary;
performance optimizations must preserve exact equality and concurrent behavior.

Adding a constraint scans existing rows under the same boundary and fails atomically
on violations. Adding/removing uses `expected_version`; dry runs report feasibility
without reserving success. Refuse field drop/rename while any local constraint names
it, and table drop while any other table declares a reference to it, even if empty;
remove the declarations explicitly first. Refuse row-access changes on constrained
or referenced tables until their constraints are removed. Namespace drop removes the
whole graph. No confirm flag bypasses constraints. Persist catalog reader-version
guards so an older writer cannot open and silently ignore constrained schemas.
Initial activation requires stopping all older writer processes before upgrading;
open-time version checks alone do not fence processes already serving requests.

Invalid declarations return `invalid_request`; valid requests violating data
constraints return `conflict`, naming the constraint and a safe repair action.
All engines and surfaces must pass conformance for NULL/predicate transitions,
composite uniqueness, int64 boundaries, timestamp offsets/precision, NULL-owner
domains across auth-mode flips, and uniform parent-deletion permission gates,
parent deletion, intra-write and batch rollback, concurrent duplicate/orphan races,
constraint migrations, restart persistence and refusal by older writers. Ship #701
in small slices: shared schema/validation; schema lifecycle/version guards; engine
enforcement; public surfaces and conformance. Intermediate slices must fail closed
on unenforced declarations. Coordinate shared-file ownership before each slice.
