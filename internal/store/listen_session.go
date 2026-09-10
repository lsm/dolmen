package store

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// listenSession is one Listen registration. Registration (listen_register.go)
// fixes the resume position P, the replay boundary R, the page chain, and
// the feed's table-lifetime labels as ONE coordinated operation, and every
// later guarantee hangs off that fixing: the replay covers seq in (P, R]
// under the registered labels, so a drop or a same-name recreate committing
// mid-replay can neither narrow the range nor mix a successor's records
// into it (§9.3). The live half grows across the slices of the 6b stack:
// the flag-only wake and the quiescing teardown (r6a), the registry join
// that orders replay against live commits (r6b), the fill pump that
// pages the durable log into the session's queue with the bound and its
// overflow teaching close (r6c, r6d), and the drain that delivers the
// queue once the replay has reported its boundary (this slice) —
// together they make the concatenation replay-then-live exactly-once.
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

	queue    []loggedChange // the interim queue: the fill pump's paged commits, delivered one at a time by the drain
	liveRead int64          // the live half's durable-log position: everything ≤ it is queued

	// firing is raised by end, in the same critical section as dead (a
	// terminal end WILL run closedFn), and lowered by fireClosed once the
	// callback returns: it brackets the one window in which a pump goroutine
	// may be running caller code that calls cancel back. A goroutine cannot
	// wait itself out, so cancel declines the join for exactly that
	// window — see cancel.
	firing bool

	// The pumps' scope: derived from the caller's context, canceled at
	// end — a session being torn down must not wait out an in-flight
	// read inside cancel.
	ctx       context.Context
	ctxCancel context.CancelFunc

	delivered atomic.Int64 // live deliveries, for periodic retention pruning (listen_drain.go)

	nextCursor      Cursor // the standing resume cursor, fixed at registration
	replayExhausted bool   // a page reached the registration boundary: the replay is done (the final publish sets it)
	dead            bool

	// The replay→live handoff, published by next() under mu. replayDone
	// is the drainer's gate: the boundary call has COMPLETED and its
	// result reached the caller — flipped in next()'s locked publish,
	// never mid-page, so an in-flight drainer can never deliver past a
	// boundary the caller has not yet seen. replayActive marks a page in
	// flight for its whole span: the drainer holds off while any page —
	// including a post-done call the handler makes — is being decided, so
	// a live record never interleaves into a caller's page turn.
	// notifyActive brackets a delivery's mint-and-notify span: the
	// parked-close machinery (the next slice) waits it out rather than
	// firing a terminal between a record and its notify, and cancel
	// declines its quiescence join for exactly the bracket — notify is
	// caller code that may reenter cancel, and no goroutine can wait
	// itself out.
	replayDone   bool
	replayActive bool
	notifyActive bool

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
// closed fires — once — with it, AFTER any in-flight delivery has
// returned: the fire waits out the delivery bracket (notifyActive — the
// record the mint just paid for must still reach the callback, and a
// consumer tearing down inside closed must never race a record being
// handed out; the contract's exposure rule is ordered BOTH ways, §6.2
// and codex P1 on #228). The bracket's owner never ends inside it — the
// drain's mint failure fires from outside its own bracket — so the wait
// always makes progress once the in-flight notify returns. A nil cause
// is the caller's cancel: the caller already knows, closed never fires
// for it (§6.2), and it skips the wait. Idempotent; the first end wins.
// The broadcast is load-bearing for the live half: a parked pump sleeps
// in cond.Wait holding nothing, and only a wake or this broadcast can
// reach it — without the broadcast, cancel's pumps.Wait below would
// block forever on a session that never saw a commit.
func (sess *listenSession) end(cause error) {
	sess.mu.Lock()
	if sess.dead {
		sess.mu.Unlock()
		return
	}
	sess.dead = true
	// The firing flag rides the SAME critical section as dead: a terminal
	// end will run closedFn, so a cancel that can observe the session dead
	// must observe the fire pending too — the join decision and callback
	// entry are one atomic step. A window between them lets an external
	// cancel decide to join a pump that then runs a callback reentering
	// cancel, and the two wait on each other.
	if cause != nil && sess.closedFn != nil {
		sess.firing = true
	}
	sess.ctxCancel() // the pumps' in-flight database work — cancel must not wait out a blocked read
	sess.cond.Broadcast()
	if cause != nil {
		// The delivery bracket: the fire lands only once the in-flight
		// delivery (its mint included) has returned. cond.Wait parks here
		// holding nothing — the bracket's clear broadcasts — and the
		// parked pumps above were already reached by end's broadcast, so
		// this wait cannot strand a teardown.
		for sess.notifyActive {
			sess.cond.Wait()
		}
	}
	sess.mu.Unlock()
	if cause != nil {
		sess.fireClosed(cause)
	}
}

// isDead is the pump's between-batches check: one lock, no broadcast, so
// a fill loop between pages observes a cancel without re-entering the
// wait.
func (sess *listenSession) isDead() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.dead
}

// fireClosed invokes the terminal callback at most once, with the end's
// cause. The callback is caller code, so a panic is recovered and logged —
// deliverCommit's write-path rule, applied here: the session is already
// ending, and a panicking close must not take the process down. end raised
// the firing flag atomically with dead; fireClosed lowers it once the
// callback returns — the callback may itself call cancel, which can never
// wait out the goroutine running it.
func (sess *listenSession) fireClosed(cause error) {
	sess.closedOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("listen closed callback panicked; session already ending",
					"namespace", sess.nsName, "table", sess.table, "panic", r)
			}
		}()
		if sess.closedFn != nil {
			defer func() {
				sess.mu.Lock()
				sess.firing = false
				sess.mu.Unlock()
			}()
			sess.closedFn(cause)
		}
	})
}

// cancel is the returned teardown: it ends the session without a closed
// callback. The registry entry comes out first (no new wakes can arrive),
// end's broadcast then reaches a parked pump, and when no terminal fire
// and no delivery is pending pumps.Wait returns only after the session's
// goroutines have exited — the use-after-free rule from notify.go's
// listener contract. The join sits OUTSIDE the once, and must: a once
// body holding a pump join would make a reentrant cancel block on the
// once behind a joiner waiting on that very pump — the two wait on each
// other. Out here the reentrant caller spends the once on teardown alone
// — which never parks — or finds it spent, and skips the join: one of
// the two no-self-wait windows is up and no goroutine can wait itself
// out. The windows: firing — a terminal fire pending or in flight
// (raised with dead in end, before the callback runs) — and notifyActive
// — a delivery's mint-and-notify span, which runs notify, caller code
// with the same reentrancy as closedFn (a subscriber unsubscribing on
// the record that completes it is the natural callback). The residue,
// then, is any cancel that observes either window — the callback may be
// pending, running, or THIS cancel's own caller; notify.go's in-flight
// rule, applied to both callbacks: treat it as possibly running, and
// know that once it returns, the pump's remainder touches nothing of the
// caller's. Idempotent.
func (sess *listenSession) cancel() {
	sess.cancelOnce.Do(func() {
		if sess.unregister != nil {
			sess.unregister()
		}
		sess.end(nil)
	})
	sess.mu.Lock()
	busy := sess.firing || sess.notifyActive
	sess.mu.Unlock()
	if !busy {
		sess.pumps.Wait()
	}
}
