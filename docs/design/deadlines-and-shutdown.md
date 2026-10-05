# Operation deadlines and shutdown

An operation's context covers database work and Dolmen's own parse/compile work. PostgreSQL
confined SQL rewriting, tree traversal and parameter casting check cancellation. Shared filter
parsing and PostgreSQL rendering do likewise. These checks preserve the existing `timeout`
and `canceled` errors and the warning that a timed-out write may or may not have committed.

The upstream PostgreSQL parser does not accept a context. Dolmen permits at most four compiler
workers across stores in one process. Admission and waiting for a worker's result are cancellable;
a canceled caller does not retain its transaction while a foreign parser call finishes. The worker
keeps its slot until it actually ends, so canceled callers cannot create an unbounded queue of
background parsers. A worker owns its mutable parse tree and name mappings. No database handle
crosses into the worker. Go traversal stops at the next cancellation check after a foreign call.

Transaction cleanup inherits the operation context, with at most five seconds for a normal
rollback. An already canceled operation rolls back with its canceled context, which makes pgx
close the connection rather than spending a fresh five seconds waiting for rollback SQL. Cleanup
uses the transaction API even when canceled: a completed transaction may already have returned its
connection to the pool, and cleanup must never touch a new owner's connection.

On shutdown, a positive `-shutdown-grace` allows requests to finish for at most that duration.
`-shutdown-grace 0` means no grace: cancel in-flight requests immediately. The old unbounded
zero-grace drain is retired. HTTP listener shutdown is awaited only within the drain deadline.

The subsequent cleanup phase has a separate fixed five-second hard cap, including closing HTTP
connections, `Store.Close`, grant-registry cleanup, and telemetry flushing. A stuck closer does
not prevent the command from returning an error and the process from exiting. Exit closes
remaining database connections; PostgreSQL rolls back their abandoned transactions and releases
locks. Cleanup inherits no unbounded behavior from any grace setting. Stdio keeps its existing
bounded request drain and worker join and shares the fixed five-second store-cleanup cap.

The CLI owns this hard-exit policy. The embedded Go facade's `Store.Close` contract is unchanged:
an embedding application's shutdown and process lifetime belong to that application.
