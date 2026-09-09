package store

import (
	"log/slog"
	"sync/atomic"
)

// Post-commit notification registry (§9.3, plan slice 4d). Change records
// are minted inside the write transaction; NOTIFICATION happens after commit
// — the two are pinned separately because they answer different questions.
// The log alone is the durability mechanism, so a lost wake is always
// harmless (§9.3): a waiter that missed it re-scans from its cursor and
// loses nothing. What this registry provides is the cheap half — waking the
// in-process waiters (wait_for's long-poll, slice 5d, and subscribe's live
// streams, 6b) the moment a commit lands, instead of on their next poll.
//
// The registry is per-namespace because the change log is (§9.3): a
// namespace feed spans tables by design, so table filtering is each
// listener's concern — the wake carries the table and the ChangeRange, and
// the range alone is all a waiter needs to re-scan (§6.2: waiters are woken
// with the range, never a materialized record slice).

// commitListener is one registered waiter. dead is set by its cancel func
// before the listener leaves the registry and re-checked immediately before
// every invocation, so once cancel returns no LATER dispatch selects it; a
// dispatch already inside fn when cancel runs still completes — the
// in-flight allowance session teardown (6b) relies on.
type commitListener struct {
	fn   func(table string, changes ChangeRange)
	dead atomic.Bool
}

// onCommit registers fn as a post-commit listener for ns and returns its
// cancel function. Cancel is idempotent and safe to call concurrently with
// dispatch. Until Listen lands (6b) the only registrants are tests.
func (s *Store) onCommit(ns string, fn func(table string, changes ChangeRange)) func() {
	l := &commitListener{fn: fn}
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.listeners == nil {
		s.listeners = map[string][]*commitListener{}
	}
	s.listeners[ns] = append(s.listeners[ns], l)
	return func() {
		l.dead.Store(true)
		s.notifyMu.Lock()
		defer s.notifyMu.Unlock()
		ls := s.listeners[ns]
		for i, cand := range ls {
			if cand == l {
				s.listeners[ns] = append(ls[:i], ls[i+1:]...)
				break
			}
		}
	}
}

// notifyCommitted wakes the listeners registered for ns. Every write path
// invokes it after tx.Commit() returns (§9.3: notification happens after
// commit) — synchronously, on the write's own goroutine, so when the write
// call returns every listener has already run. A write that minted no
// records wakes nobody: the log head never moved, so there is nothing for a
// waiter to see — an idempotent replay already woke them for the original
// commit, and an update or delete that matched nothing changed nothing.
//
// The listener list is snapshotted under the registry mutex and invoked
// outside it, so a slow listener never blocks registration or another
// namespace's writes.
func (s *Store) notifyCommitted(ns, table string, changes ChangeRange) {
	if changes.Count == 0 {
		return
	}
	s.notifyMu.Lock()
	listeners := append([]*commitListener(nil), s.listeners[ns]...)
	s.notifyMu.Unlock()
	for _, l := range listeners {
		if l.dead.Load() {
			continue
		}
		deliverCommit(l, ns, table, changes)
	}
}

// deliverCommit invokes one listener, write-safely: the write it describes
// has ALREADY committed, so a panic propagating out of fn would fail a
// committed write — reporting an error for data the client cannot un-write.
// It is recovered and logged instead (§9.3: notification is best-effort, and
// the durable log recovers everything a dropped wake signaled). One recover
// per listener, so a panicking listener never skips the ones after it.
func deliverCommit(l *commitListener, ns, table string, changes ChangeRange) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("post-commit listener panicked; write already committed, wake dropped",
				"namespace", ns, "table", table,
				"changes_first", changes.First, "changes_last", changes.Last,
				"panic", r)
		}
	}()
	l.fn(table, changes)
}
