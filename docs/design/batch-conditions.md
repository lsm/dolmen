# Conditional Batch writes

**Status: implemented (#703, #741).**
This extends [Batch writes](batch-writes.md); its atomicity, ordering, limits,
authorization and result shapes remain the contract unless noted below.

## Decision and example

Add optional `expected_rows` to Batch `update` and `delete` writes. A caller reads
state, decides locally, then puts the expected old values in the existing filter.
The engine evaluates that predicate and checks its row count inside the batch's
write boundary. A mismatch aborts the entire batch, including earlier writes.
This supplies compare-and-set without a callback holding the writer across client
code, a SQL escape hatch, or a second transaction API.

```json
{"namespace":"work","writes":[
  {"kind":"update","table":"jobs",
   "filter":"id = ? AND version = ? AND status = ?","args":[42,7,"ready"],
   "set":{"version":8,"status":"claimed"},"expected_rows":1},
  {"kind":"insert","table":"events","records":[{"job_id":42,"event":"claimed"}]}
]}
```

Only a successful claim can commit the event. Two callers using version 7 cannot
both claim it: one changes the version; the other matches zero rows and receives
`conflict`. On conflict, re-read and recompute the decision; do not blindly retry
old values. `version` is an ordinary application field, not an implicit column.

## Contract

- Omission preserves today's zero-match success. An explicit integer from 0 to
  the existing 1,000-row batch budget asserts an exact count; reject null,
  negative, fractional, out-of-range or wrong-kind values as `invalid_request`,
  with the write index. Zero means “this mutation must match nothing”, not “skip
  this write”. For update use `updated`; for delete use `matched`. These count
  matched rows, including unchanged update values.
- Check the mutation's authoritative in-boundary count before accepting its
  result or committing, never an earlier read-pool count. A mismatch returns
  `conflict` / Go `ErrConflict`, for example `writes[0]: expected_rows mismatch;
  re-read state and retry the decision`. No partial result, data, change-feed
  commit or new idempotency record survives. Existing validation, authorization,
  mutation errors and safety limits retain their classes; the expectation does
  not bypass delete confirmation or the aggregate row budget.
- Writes remain ordered and later predicates see earlier writes. Results do not
  gain a field. `expected_rows` is Batch-only: add it to the derived update/delete
  input schemas after derivation, not to single-operation schemas or upsert kinds
  whose insert branch would obscure whether a comparison succeeded.
- Reuse each write's grants, visible-row scope, filter restrictions and secret
  handling. Hidden rows never enter the comparison. Do not return their values
  or introduce a read permission bypass. A scoped zero does not assert global
  absence, and the condition provides no guarantee about state after commit.
- Include present expectations in idempotency hashing, including secret-bearing
  batches; keep omitted expectations compatible with existing hashes. An exact
  successful replay returns its saved result without comparing again; a changed
  expectation under the same key conflicts. Failed comparisons consume no key.
  Go adds `ExpectedRows *int64` to `BatchWrite`; nil is omission, a pointer to zero
  is an assertion. HTTP, MCP and Go use the same engine check.

## Execution and proof

Keep existing protection: SQLite's shared Batch writer transaction; PostgreSQL's
carried transaction and namespace `FOR NO KEY UPDATE` lock; lakehouse's exclusive
namespace admission and rollback journal. Check before commit on each engine;
no new locking policy or isolation promise. Embedding/file preparation retains
its existing costs and rollback rules; external provider work is not undone.

Implement #703 in independently passing 100–200-production-line PRs: (1) shared
validation/hash/count rules and SQLite, (2) PostgreSQL and lakehouse parity,
(3) wire schemas/decoding, Go facade and served documentation. Backends not yet
implementing the check must reject a present expectation, never ignore it. Earlier
PRs reference #703; only the final parity-complete PR closes it. Every behavior slice starts
with a failing contract and includes applicable conformance coverage.

Prove matched/stale/missing-row CAS, zero versus omission, no-op update counts,
prior-write rollback with no feed/key residue, ordered intra-batch visibility,
exact replay after state changes, changed-expectation conflicts, scoped/secret
filters, and two simultaneous version-7 claims with exactly one committed event.
Run engine race tests and HTTP/MCP/Go parity on synthetic stores. This does not add
arbitrary transactional reads, result substitution, branching, cross-namespace
transactions, constraints, import/migration behavior, or serialization tuning.
