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
Go, HTTP and MCP. This is a managed invariant, not an `UpsertByKey` matching hint.

## Meaning

- **Unique:** one to eight distinct declared scalar fields (`string`, `text`,
  `number`, `boolean`, `timestamp`); composite secondary uniqueness is supported.
  Rows participate only when `where` evaluates TRUE; FALSE/NULL do not participate.
  If any key component is NULL, duplicates are allowed. Compare coerced values:
  bytewise case-sensitive text, normalized timestamps and exact numeric equality
  (1 equals 1.0; comparing integers must not round them through float64). Reject
  non-finite constrained numbers. JSON, vector, secret and file keys are excluded.
- **Predicate:** a bounded subset of the shared filter AST: scalar column/literal
  comparisons, `IS [NOT] NULL`, parentheses and AND/OR/NOT. No parameters, functions,
  SQL, subqueries, hidden columns or other tables. Predicate fields use the same
  scalar types as keys. Existing filter length/depth limits apply. Parse once and
  evaluate with shared typed semantics; never interpret backend-specific SQL.
- **Foreign key:** one declared `number` field references another table's system
  `id` in the same namespace, including a self-reference. Non-NULL values must be
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
An absent parent and another owner's parent produce the same failure.

Plain-table uniqueness and RESTRICT checks could reveal unreadable rows. Require
`read` in addition to the mutation verb on each plain table whose rows a constraint
check consults (including referencing tables for parent deletion), checked before
examining data. Own-row checks require a standing own-row read on every consulted table (`read`
or a data verb, per the existing own-row rules); a table-wide mutation must also
be authorized to read every affected domain. Constraint schema changes require
`schema` and `read` on all affected tables. This amends the permission contract
only for constrained operations; unconstrained operations are unchanged.
No error exposes conflicting values, row IDs, owner labels or hidden row counts.

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
composite uniqueness, int64 boundaries, owner isolation and permission gates,
parent deletion, intra-write and batch rollback, concurrent duplicate/orphan races,
constraint migrations, restart persistence and refusal by older writers. Ship #701
in small slices: shared schema/validation; schema lifecycle/version guards; engine
enforcement; public surfaces and conformance. Intermediate slices must fail closed
on unenforced declarations. Coordinate shared-file ownership before each slice.
