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

	// A terminal PARKS instead of firing the moment its end lands (the
	// exposure rule's serialization point, §6.2): closed must never precede
	// a record the session already handed out — not an in-flight notify's
	// record, not a page the caller is still deciding — so the cause waits
	// for the quiescent exit of a pump goroutine (flushParkedClose).
	// pendingDrainClose is the queue-OWNED variant: a terminal whose
	// already-queued prefix must deliver first, fired only by the drainer
	// once the queue empties.
	pendingClose      error // parked by end; fired by a pump's deferred flush (or inline on a never-launched session)
	pendingDrainClose error // armed by the queue's owner; fired by the drainer at the empty queue

	// pumpsLaunched is set once, under mu, at Listen's launch site: after
	// it, the pumps' deferred flushes own every parked cause; before it
	// (the direct fixtures, a session whose goroutines never started) no
	// flush can ever run, so end fires inline — the direct-end path's
	// fire contract holds either way.
	pumpsLaunched bool

	// firing is raised by fireClosed, under mu, immediately before the
	// closedFn callback runs, and lowered once it returns: it brackets
	// exactly the one window in which a pump goroutine may be running
	// caller code that calls cancel back — the callback's own execution,
	// never a pending fire (a parked cause waiting its flush runs with the
	// flag down, so an external cancel behind a blocked delivery still
	// joins). A goroutine cannot wait itself out, so cancel declines the
	// join for exactly that window — see cancel.
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
	// is the drainer's gate: it flips in next()'s locked publish — the
	// last serialized step of the caller's boundary call, never mid-page
	// — so no later page can interleave and no record delivers past a
	// boundary a page is still deciding. The first live callback may
	// still arrive in the boundary call's RETURN window: every gate the
	// engine could hold (this lock, the flight permit) releases in that
	// same window, and notify always runs on an engine goroutine, so the
	// caller's replay-to-live transition is concurrent by design — the
	// replay's own records were all returned by the final-records page,
	// and the concatenation's cursor order is unaffected. replayActive
	// marks a page in flight for its whole span: the drainer holds off
	// while any page — including a post-done call the handler makes — is
	// being decided, so a live record never interleaves into a caller's
	// page turn. notifyActive brackets a delivery's mint-and-notify span:
	// the parked-close machinery (the next slice) waits it out rather
	// than firing a terminal between a record and its notify, and an
	// external cancel's quiescence join spans it — the callback has
	// returned before cancel does (notify itself carries the caller rule
	// cancel documents: never cancel synchronously from inside it).
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
// it PARKS here — closed fires later, once, from a pump goroutine's
// quiescent exit (flushParkedClose), never inline on the end caller: a
// terminal must not precede a record the session already handed out, and
// the engine halves end sessions from mid-loop positions where a delivery
// or a page can still be in flight. The ONE exception is the
// never-launched session (the direct fixtures): with no pump ever
// reaching a deferred flush, the fire happens inline — nothing can be in
// flight without a drain. A nil cause is the caller's cancel:
// the caller already knows, and closed never fires for it (§6.2).
// Idempotent; the first end wins. The broadcast is load-bearing for the
// live half: a parked pump sleeps in cond.Wait holding nothing, and only
// a wake or this broadcast can reach it — without the broadcast,
// cancel's pumps.Wait below would block forever on a session that never
// saw a commit.
func (sess *listenSession) end(cause error) {
	sess.mu.Lock()
	if sess.dead {
		sess.mu.Unlock()
		return
	}
	sess.dead = true
	// The park rides the SAME critical section as dead, and the first
	// cause parked wins: the flush (from a pump's deferred, counted exit)
	// waits out any in-flight delivery and page BEFORE firing, so the
	// exposure rule's serialization lands at the flush — never here on
	// the end caller. firing is NOT raised here: the flag brackets
	// exactly the closedFn callback's execution (fireClosed raises it,
	// under mu, immediately before the callback runs), so a cancel
	// arriving while the fire is still PARKED — waiting its bracket
	// behind a blocked notify — takes the quiescence join and returns
	// only after the whole teardown; the carve-out never covers a pending
	// fire (codex P1 on #228, thread r3984384330).
	if cause != nil && sess.pendingClose == nil {
		sess.pendingClose = cause
	}
	sess.ctxCancel() // the pumps' in-flight database work — cancel must not wait out a blocked read
	sess.cond.Broadcast()
	if cause != nil && !sess.pumpsLaunched {
		// The not-via-Listen world: no production launch marked the
		// session, so no pump's deferred flush is guaranteed — the fire
		// happens HERE. It still honors the exposure rule: a
		// directly-launched pump (a fixture that skipped Listen) can be
		// mid-delivery or mid-page, so the inline fire waits the same
		// brackets the flush does; the genuinely never-launched session
		// has no drain and the wait is a no-op. The first parked cause is
		// the one that fires.
		cause = sess.pendingClose
		sess.pendingClose = nil
		for sess.replayActive || sess.notifyActive {
			sess.cond.Wait()
		}
		sess.mu.Unlock()
		sess.fireClosed(cause)
		return
	}
	sess.mu.Unlock()
}

// flushParkedClose fires a parked terminal from a pump's exit path, once
// no replay page and no delivery is in flight. The park is the exposure
// rule's serialization point (§6.2): closed must never precede a record
// the session already handed out — an in-flight notify has returned
// before its pump reaches this deferred flush, and the other pump's flush
// waits out the delivery flag — and a page minted while the session was
// ending may still be returning, so the parked close waits for it rather
// than cutting past it. The wait exists to serialize the FIRE: with
// nothing parked (a caller-initiated cancel arms no cause) it is skipped,
// keeping teardown off unrelated replay work — a Next blocked on its own
// context behind the namespace's write connection must not hang cancel
// when no callback is owed (codex P2 on #229, thread r3984531845). The
// FIRST cause parked wins; later ends are already absorbed by the dead
// flag. The flush runs on the counted pump — before its pumps.Done — so
// cancel either waits out the whole fire or, inside the callback itself,
// declines the join on the firing flag.
func (sess *listenSession) flushParkedClose() {
	sess.mu.Lock()
	for sess.pendingClose != nil && (sess.replayActive || sess.notifyActive) {
		sess.cond.Wait()
	}
	cause := sess.pendingClose
	sess.pendingClose = nil
	sess.mu.Unlock()
	if cause != nil {
		sess.fireClosed(cause)
	}
}

// fireClosed invokes the terminal callback at most once, with the end's
// cause. The callback is caller code, so a panic is recovered and logged —
// deliverCommit's write-path rule, applied here: the session is already
// ending, and a panicking close must not take the process down. firing
// raises HERE, under mu, immediately before the callback runs, and lowers
// once it returns: the flag brackets exactly the callback's EXECUTION —
// the one window a cancel cannot join, because the callback may itself be
// that cancel's caller (no goroutine waits itself out). The raise and any
// join decision serialize on mu, so a cancel that reads firing=false has
// taken the join and waits the fire out on the counted pump; a pending
// fire — one still waiting its bracket — is never covered (codex P1 on
// #228, thread r3984384330).
func (sess *listenSession) fireClosed(cause error) {
	sess.closedOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("listen closed callback panicked; session already ending",
					"namespace", sess.nsName, "table", sess.table, "panic", r)
			}
		}()
		if sess.closedFn != nil {
			sess.mu.Lock()
			sess.firing = true
			sess.mu.Unlock()
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
// is pending pumps.Wait returns only after the session's goroutines have
// exited — the use-after-free rule from notify.go's listener contract:
// once cancel returns, no callback will ever run again. The join sits
// OUTSIDE the once, and must: a once body holding a pump join would make
// a closedFn that reenters cancel block on the once behind a joiner
// waiting on that very pump — the two wait on each other. Out here the
// reentrant caller spends the once on teardown alone — which never parks
// — or finds it spent, and skips the join on the FIRING flag: a terminal
// fire pending or in flight, a ONE-TIME window at the session's end,
// raised with dead in end before the callback runs (the residue: treat
// closedFn as possibly running; once it returns, the pump's remainder
// touches nothing of the caller's).
//
// notify gets NO carve-out, though it is caller code with the same
// reentrancy: a delivery bracket is CONTINUOUS on a busy stream, so a
// notifyActive term would void the quiescence guarantee for every
// external cancel — no session-wide flag distinguishes the callback's
// own goroutine from an outside joiner, and the guarantee is the
// contract (codex P1s on #228, threads r3984073040/r3984335493). The
// rule is therefore the caller's, as the reference states it: cancel
// must not be called synchronously from inside notify — the wait would
// sit on the calling goroutine's own pump. A notify that unsubscribes
// defers (`go cancel()`) or cancels after its callback returns; an
// external cancel waits any in-flight delivery out. Idempotent.
func (sess *listenSession) cancel() {
	sess.cancelOnce.Do(func() {
		if sess.unregister != nil {
			sess.unregister()
		}
		sess.end(nil)
	})
	sess.mu.Lock()
	firing := sess.firing
	sess.mu.Unlock()
	if !firing {
		sess.pumps.Wait()
	}
}
