package store

import (
	"log/slog"
	"sync"
)

// listenSession is one Listen registration. Registration (listen_register.go)
// fixes the resume position P, the replay boundary R, the page chain, and
// the feed's table-lifetime labels as ONE coordinated operation, and every
// later guarantee hangs off that fixing: the replay covers seq in (P, R]
// under the registered labels, so a drop or a same-name recreate committing
// mid-replay can neither narrow the range nor mix a successor's records
// into it (§9.3). The live half grows across the slices of the 6b stack:
// the flag-only wake and the quiescing teardown (r6a), then the registry
// join that orders replay against live commits (r6b); the fill pump that
// pages the durable log into the session's queue, the drain that
// delivers it, and the queue's bound follow — together they make the
// concatenation replay-then-live exactly-once.
type listenSession struct {
	s      *Store
	n      *nsDB // the namespace instance registered on — never a successor's
	nsName string
	table  string // "" = the namespace-wide feed
	nsGen  [16]byte
	// TODO(8c): nsGen is ignored while auth is off — slice 8c verifies the
	// namespace-lifetime guard atomically with registration, exactly like
	// Query. The nsDB binding below already refuses the successor case
	// structurally: the session's pools die with the dropped namespace.

	liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool)
	notify    func(ChangeRecord)
	closedFn  func(cause error)

	// Registration-fixed replay state — immutable once Listen returns,
	// except the chain, which rotates at its retention cap.
	position    int64        // resume position P: the client's `from`, resolved
	boundary    int64        // registration boundary R: the replay is (P, R]
	outstanding int64        // loss-check baseline: rows the log retained in (P, R] AT REGISTRATION — holes included; the pages' promise
	chain       *cursorChain // the page chain every token the session mints rides
	feed        *changeFeed  // nil on the namespace feed; the table feed's labels at registration

	mu         sync.Mutex
	cond       *sync.Cond     // the pump's signal: a wake (a commit landed) or the end broadcast
	flight     chan struct{}  // the single-flight permit for Next: one page+publish in the air at a time, acquired cancellably (listen_page.go)
	woken      bool           // the registry's flag: a commit landed; the pump clears it as it takes the work
	pumps      sync.WaitGroup // the session's own goroutines; cancel waits it empty before returning
	unregister func()         // leaves the commit registry; nil when the session never joined (the direct fixtures)

	nextCursor      Cursor // the standing resume cursor, fixed at registration
	replayExhausted bool   // a page reached the registration boundary: the replay is done (the final publish sets it)
	dead            bool

	closedOnce sync.Once
	cancelOnce sync.Once
}

// cursor returns the session's standing resume cursor — what a caller that
// stops paging, or never pages, resumes from.
func (sess *listenSession) cursor() Cursor {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.nextCursor
}

// end terminates the session. A non-nil cause is an ENGINE-initiated end:
// closed fires — once — with it. A nil cause is the caller's cancel: the
// caller already knows, and closed never fires for it (§6.2). Idempotent;
// the first end wins. The broadcast is load-bearing for the live half: a
// parked pump sleeps in cond.Wait holding nothing, and only a wake or
// this broadcast can reach it — without the broadcast, cancel's
// pumps.Wait below would block forever on a session that never saw a
// commit.
func (sess *listenSession) end(cause error) {
	sess.mu.Lock()
	if sess.dead {
		sess.mu.Unlock()
		return
	}
	sess.dead = true
	sess.cond.Broadcast()
	sess.mu.Unlock()
	if cause != nil {
		sess.fireClosed(cause)
	}
}

// fireClosed invokes the terminal callback at most once, with the end's
// cause. The callback is caller code, so a panic is recovered and logged —
// deliverCommit's write-path rule, applied here: the session is already
// ending, and a panicking close must not take the process down.
func (sess *listenSession) fireClosed(cause error) {
	sess.closedOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("listen closed callback panicked; session already ending",
					"namespace", sess.nsName, "table", sess.table, "panic", r)
			}
		}()
		if sess.closedFn != nil {
			sess.closedFn(cause)
		}
	})
}

// cancel is the returned teardown: it ends the session without a closed
// callback and leaves nothing of it running. The registry entry comes out
// first (no new wakes can arrive), end's broadcast then reaches a parked
// pump, and pumps.Wait returns only after the session's goroutines have
// exited — the use-after-free rule from notify.go's listener contract.
// Idempotent.
func (sess *listenSession) cancel() {
	sess.cancelOnce.Do(func() {
		if sess.unregister != nil {
			sess.unregister()
		}
		sess.end(nil)
		sess.pumps.Wait()
	})
}
