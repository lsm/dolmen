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

The shutdown policy decision remains pending: after the configured HTTP drain grace, allow at
most five seconds for cancellation and store cleanup before the command exits nonzero. Process
exit closes any remaining database connections; PostgreSQL rolls back their open transactions.
Keep `-shutdown-grace 0` as the existing explicit unbounded-drain setting. The cleanup cap must be
approved before implementation because it changes the documented shutdown contract.
