package store

// The live half — slice 6b (r6a). This file grows the live producer over
// the next slices of the 6b stack; this slice is the scaffolding it runs
// on: the flag-only wake the commit registry will call, and — on the
// session — the teardown rules that make a session with a live half safe
// to drop. The fill pump that pages the durable log into the session's
// queue, the drain that delivers it, and the queue's bound land in the
// following slices.

// wake is the registry callback: it runs on COMMITTING writers' goroutines,
// so it does nothing but raise the fill flag — no database access, no
// filtering, no client I/O. The wake is the latency half only; the durable
// log the filler re-reads is the delivery guarantee, which is what makes a
// lost or racing wake harmless.
func (sess *listenSession) wake(table string, changes ChangeRange) {
	sess.mu.Lock()
	sess.woken = true
	sess.cond.Broadcast()
	sess.mu.Unlock()
}
