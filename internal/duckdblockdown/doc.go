// Package duckdblockdown is the spike from docs/design/lakehouse-plan.md §2.5: a
// minimal helper that starts a DuckDB CLI locked to one namespace's data
// directory, and the tests that attack it.
//
// The tests are the deliverable. The helper exists only so there is something
// to attack, and it is deliberately not engine code: nothing here is wired into
// store.Engine, and no operation reaches it. What the spike decides is whether
// a lakehouse `query` over a DuckDB process can be confined by DuckDB's own
// settings, and whether the stdio protocol can keep CLI dot-commands from being
// reachable by caller SQL.
//
// The result, including the two findings that decide the lane, is in
// docs/design/lakehouse-plan.md §2.4 and §2.5.
package duckdblockdown
