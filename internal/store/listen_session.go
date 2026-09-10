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
// into it (§9.3). Later slices of the 6b stack grow the session with the
// live half — the registry join, the fill/drain pumps, the bounded queue —
// which is what makes the concatenation replay-then-live exactly-once.
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

	mu              sync.Mutex
	flight         chan struct{} // the single-flight permit for Next: one page+publish in the air at a time, acquired cancellably (listen_page.go)
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
// the first end wins. The wait and callback rules for the pump goroutines
// arrive with the live half of the 6b stack.
func (sess *listenSession) end(cause error) {
	sess.mu.Lock()
	if sess.dead {
		sess.mu.Unlock()
		return
	}
	sess.dead = true
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
// callback. Nothing runs concurrently in this half — the session's only
// goroutine is the caller's own Next — so quiescence is the dead flag
// itself; the live half's registry entries and pump goroutines arrive with
// the next slice of the 6b stack, and cancel will unregister and wait them
// out before returning (the use-after-free rule from notify.go's listener
// contract).
func (sess *listenSession) cancel() {
	sess.cancelOnce.Do(func() {
		sess.end(nil)
	})
}
