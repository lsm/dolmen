package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
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
//
// The registry is deliberately NOT namespace-lifecycle-aware: a wake is
// advisory and carries no lifetime binding, so listeners registered before
// a DropNamespace may be woken by a recreated successor's commits. That is
// harmless under §9.3 — the waiter's authorized re-scan of the durable log,
// never the wake itself, is what delivers records — and 6b's Listen binds
// its sessions to nsGen and cancels them at drop.

// commitListener is one registered waiter. dead is set by its cancel func
// before the listener leaves the registry and re-checked immediately before
// every invocation, so once cancel returns, a dispatch that has not yet
// passed that check will never invoke fn. What cancel does NOT guarantee is
// the end of in-flight dispatch: one already inside fn completes, and one
// that passed its dead-check just before cancel may still ENTER fn after
// cancel returned — the check and the call are two steps, and dispatch holds
// no lock across them. A 6b session teardown must treat fn as possibly
// running until it can prove quiescence; releasing what fn captures on
// cancel's return alone is a use-after-free.
type commitListener struct {
	fn   func(table string, changes ChangeRange)
	dead atomic.Bool
}

// onCommit registers fn as a post-commit listener for ns and returns its
// cancel function. Cancel is idempotent and safe to call concurrently with
// dispatch. Until Listen lands (6b) the only registrants are tests.
//
// fn runs synchronously on the writing goroutine, after its commit, and must
// honor three rules the registry cannot enforce: it may be entered
// CONCURRENTLY by several writers committing to the same namespace, so it
// must be safe for simultaneous invocation; it must not runtime.Goexit
// (t.Fatal — recover cannot catch it, and the write's goroutine would die
// mid-response); and it must not synchronously write to the same namespace,
// which re-enters dispatch and recurses without bound.
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

// ---------------------------------------------------------------------------
// Listen — slice 6b, §6.2/§9.3's atomic register-and-replay.

var (
	// ErrListenOverflow is the teaching close for a subscriber whose own
	// visible traffic outran its drain: the session's bounded buffer filled
	// (§6.2, §9.3). The stream ends and the client resumes from its last
	// persisted cursor — the durable log is the durability mechanism; the
	// buffer never is.
	ErrListenOverflow = errors.New("subscription buffer overflow: the subscriber drained slower than commits arrived")

	// ErrListenRevoked closes a session whose liveAuthz returned ok=false
	// mid-stream (§6.2): the standing read's authorization was revoked or
	// narrowed, and the next event — replay or live — teaching-closes the
	// stream instead of exposing a record the caller may no longer see.
	ErrListenRevoked = errors.New("subscription authorization revoked mid-stream")

	// ErrListenLifetimeEnded closes a session whose feed target ended under
	// it: the subscribed table was dropped (a table feed follows ONE table
	// lifetime, §9.3 — a successor of the same name is a different feed), or
	// the namespace was dropped or replaced. The session is bound to the
	// namespace instance it registered on — a recreated namespace restarts
	// the change sequence, and a predecessor's session must never follow into
	// the successor (§9.3's cursor-lifetime rule, applied to streams).
	ErrListenLifetimeEnded = errors.New("subscription target's lifetime ended")
)

// listenQueueBound caps the records buffered for one subscriber: the interim
// commits landing while the replay drains, plus live records queued behind a
// slow client (§6.2). It is deliberately a multiple of the page bound — a
// single bulk commit the size of a page must not overflow a fresh subscriber
// — and only VISIBLE records ever occupy it (admission precedes enqueue), so
// it can be tripped by nothing but the subscriber's own traffic.
const listenQueueBound = 8 * MaxChangesPageLimit

// listenSession is one Listen registration: the replay half (Next, driven by
// the caller's goroutine), and the live half (fill and drain goroutines the
// engine runs until cancel or close). Registration fixes three things as one
// coordinated operation — the listener's registry entry, the replay boundary
// R, and the feed's table-lifetime labels — and every later guarantee hangs
// off that fixing:
//
//   - EXACTLY ONCE across the boundary: replay covers seq in (P, R] and the
//     live half reads seq > R, disjoint by construction. The listener joins
//     the registry BEFORE the boundary transaction reads the log head, and a
//     record can only take seq > R by committing after that transaction —
//     SQLite serializes writers through the same rw connection, so its commit
//     (and therefore its post-commit notifyCommitted, which runs after the
//     commit returns) necessarily observes the registration. A record with
//     seq <= R committed before the read; its late wake finds nothing new to
//     fill. No event can be both replayed and pushed, and none can fall
//     between the halves.
//
//   - ORDER: the concatenation replay-then-live is cursor order. The live
//     read advances monotonically over the durable log — never materializing
//     from the wake itself — so out-of-order wake dispatch (two writers'
//     notifyCommitted racing) cannot reorder delivery.
//
//   - NO WRITER BLOCKAGE: the wake callback only sets a flag and broadcasts;
//     every database read, every authorization callback, and every client
//     write happen on the session's own goroutines, never the writer's.
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

	// Registration-fixed replay state — immutable once Listen returns.
	position int64        // resume position P: the client's `from`, resolved
	boundary int64        // registration boundary R: replay is (P, R], live is > R
	chain    *cursorChain // the page chain every token the session mints rides
	feed     *changeFeed  // nil on the namespace feed; the table feed's labels at registration

	mu              sync.Mutex
	cond            *sync.Cond
	queue           []ChangeRecord // bounded by listenQueueBound, in seq order
	liveRead        int64          // last live position read from the log, visible or not
	woken           bool
	replayDone      bool // Next has drained the replay: the drainer may deliver
	replayExhausted bool
	nextCursor      Cursor
	dead            bool

	closedOnce sync.Once
	cancelOnce sync.Once
	pumps      sync.WaitGroup
	unregister func()
}

// Listen implements the engine-declared notification capability (§6.2, §9.3):
// the listener registration and the replay boundary are fixed as ONE
// coordinated operation, the replay pages out through ChangeReplay.Next, and
// once the caller drains it the session pushes live records through notify.
// closed fires at most once, only when the ENGINE ends the session
// (ErrListenOverflow, ErrListenRevoked, ErrListenLifetimeEnded, or an engine
// failure) — never for a caller-initiated cancel. The returned cancel is
// idempotent and returns only once the session is quiescent: after it
// returns, no callback will ever run again (notify.go's teardown rule).
//
// TODO(9d): while auth is off the SSE handler passes a nil liveAuthz (no
// per-event filter); the admission rules below are live the moment a slice
// wires a real re-resolver in.
func (s *Store) Listen(ctx context.Context, nsName, table string, from Cursor, nsGen [16]byte, liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool), notify func(ChangeRecord), closed func(cause error)) (*ChangeReplay, func(), error) {
	if notify == nil {
		return nil, nil, invalidf("listen: notify callback is required")
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, nil, err
	}
	sess := &listenSession{
		s: s, n: n, nsName: nsName, table: table, nsGen: nsGen,
		liveAuthz: liveAuthz, notify: notify, closedFn: closed,
	}
	sess.cond = sync.NewCond(&sess.mu)

	// Atomic register-and-replay, ordering half one: the listener joins the
	// registry BEFORE the boundary transaction runs (see the session comment
	// for the exactly-once argument). Failure past this point must leave the
	// registry as it found it.
	sess.unregister = s.onCommit(nsName, sess.wake)
	committed := false
	defer func() {
		if !committed {
			sess.unregister()
		}
	}()

	now := time.Now()
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	// The table feed's target, fixed at registration: the table exists now,
	// and the labels pin WHICH lifetime the feed follows for its whole life —
	// a same-named successor is a different feed, and the session ends rather
	// than silently narrowing to it (ErrListenLifetimeEnded).
	if table != "" {
		var ferr error
		if sess.feed, ferr = changeFeedOf(ctx, tx, nsName, table); ferr != nil {
			return nil, nil, ferr
		}
	}

	// The resume position and its page chain — ChangesSince's exact rules
	// (zero = current head, "begin" = the retained-history boundary, anything
	// else an opaque token with feed binding and retention enforced before
	// the position is honored).
	switch {
	case from == "":
		if sess.position, err = changeHead(ctx, tx); err != nil {
			return nil, nil, err
		}
		sess.chain = newCursorChain(now, sess.position)
	case from == CursorBegin:
		if sess.position, err = changeBegin(ctx, tx, now, s.changeRetention); err != nil {
			return nil, nil, err
		}
		sess.chain = newCursorChain(now, sess.position)
	default:
		var row cursorRow
		if row, err = resolveCursorToken(ctx, tx, now, s.changeRetention, from, table); err != nil {
			return nil, nil, err
		}
		sess.position = row.Position
		sess.chain = &cursorChain{ID: row.ChainID, Origin: row.ChainOrigin, Start: row.ChainStart}
	}

	// Ordering half two: the boundary. The immediate write lock this
	// transaction holds means no writer can interleave between the head read
	// and the commit — R is the head of a serial-observability point (§0.6),
	// and everything after it is live-only.
	if sess.boundary, err = changeHead(ctx, tx); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	committed = true

	sess.pumps.Add(2)
	go sess.fill()
	go sess.drain()
	s.trackSession(sess)
	return &ChangeReplay{Next: sess.next}, sess.cancel, nil
}

// trackSession registers the session for LIFECYCLE wakes — a different event
// from the commit wake onCommit serves: "new commits exist" vs "the feed's
// world changed; look again". A drop leaves no further commits to wake a
// session with, yet a dropped namespace's streams must end at the drop
// (notify.go's binding note), so DropNamespace and DropTable nudge every
// session of the namespace and each session's next fill decides its own
// fate. notifyMu — the same lock as the commit registry — keeps a lifecycle
// wake from interleaving with registration/cancel bookkeeping.
func (s *Store) trackSession(sess *listenSession) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.listenSessions == nil {
		s.listenSessions = map[string][]*listenSession{}
	}
	s.listenSessions[sess.nsName] = append(s.listenSessions[sess.nsName], sess)
}

// untrackSession removes a cancelled session from the lifecycle registry.
func (s *Store) untrackSession(sess *listenSession) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	sessions := s.listenSessions[sess.nsName]
	for i, cand := range sessions {
		if cand == sess {
			s.listenSessions[sess.nsName] = append(sessions[:i], sessions[i+1:]...)
			break
		}
	}
}

// wakeListenSessions nudges every session registered on the namespace — the
// lifecycle wake (see trackSession). The nudge itself carries no state, so
// it is safe on any goroutine, under any store lock: each woken session
// re-derives the truth from the durable world on its own pump.
func (s *Store) wakeListenSessions(ns string) {
	s.notifyMu.Lock()
	sessions := append([]*listenSession(nil), s.listenSessions[ns]...)
	s.notifyMu.Unlock()
	for _, sess := range sessions {
		sess.wake("", ChangeRange{})
	}
}

// wake is the registry callback: it runs on COMMITTING writers' goroutines,
// so it does nothing but raise the fill flag — no database access, no
// filtering, no client I/O (notify.go's listener rules). The wake is the
// latency half only; the durable log the filler re-reads is the delivery
// guarantee, which is what makes a lost or racing wake harmless.
func (sess *listenSession) wake(table string, changes ChangeRange) {
	sess.mu.Lock()
	sess.woken = true
	sess.cond.Broadcast()
	sess.mu.Unlock()
}

// fill is the session's live read half: on every wake it pages the durable
// log forward from liveRead, admits each record through the live
// authorization, mints the admitted records' cursors, and queues them for
// the drainer. It shares the rw pool with Next and the write paths, so its
// transactions serialize with them; it never holds sess.mu across database
// work. A feed whose target ended — table dropped, or its labels no longer
// the registered lifetime's — ends the session instead of silently
// narrowing; so does a queue past listenQueueBound (the teaching reconnect)
// and any fill failure.
func (sess *listenSession) fill() {
	defer sess.pumps.Done()
	defer sess.recoverPump("fill")
	for {
		sess.mu.Lock()
		for !sess.dead && !sess.woken {
			sess.cond.Wait()
		}
		if sess.dead {
			sess.mu.Unlock()
			return
		}
		sess.woken = false
		sess.mu.Unlock()

		for {
			if sess.isDead() {
				return
			}
			visible, read, ferr := sess.fillBatch()
			if ferr != nil {
				sess.end(ferr)
				return
			}
			if len(visible) > 0 {
				sess.mu.Lock()
				if sess.dead { // ended while the batch was in flight
					sess.mu.Unlock()
					return
				}
				sess.queue = append(sess.queue, visible...)
				over := len(sess.queue) > listenQueueBound
				sess.cond.Broadcast()
				sess.mu.Unlock()
				if over {
					sess.end(ErrListenOverflow)
					return
				}
			}
			if read < MaxChangesPageLimit {
				break // drained to the head; wait for the next wake
			}
		}
	}
}

// fillBatch reads one bounded batch of live records, admits each through
// the session's live authorization OUTSIDE any transaction (the 9d
// re-resolver may itself read through this engine; invoking caller code
// inside the namespace's write lock could deadlock against it), then mints
// the admitted records' cursors and prunes retention in a second
// transaction. liveRead advances past every record read, visible or not: an
// invisible record is delivered never, but its position is consumed exactly
// once.
func (sess *listenSession) fillBatch() (visible []ChangeRecord, read int, err error) {
	ctx := context.Background() // the pump outlives the request; cancel is its stop signal
	now := time.Now()
	scanned, from, rerr := sess.readBatch(ctx)
	if rerr != nil {
		return nil, 0, rerr
	}
	sess.mu.Lock()
	if len(scanned) > 0 {
		sess.liveRead = scanned[len(scanned)-1].seq
	}
	sess.mu.Unlock()

	var admitted []loggedChange
	for _, lc := range scanned {
		vis, revoked := sess.admit(lc.rec)
		if revoked {
			sess.end(ErrListenRevoked)
			return nil, 0, nil
		}
		if vis {
			admitted = append(admitted, lc)
		}
	}
	if len(admitted) == 0 {
		return nil, len(scanned), nil // nothing visible: no tokens, no second transaction
	}
	records, _, merr := sess.mint(ctx, now, admitted, from)
	if merr != nil {
		return nil, 0, sess.fillErr(merr)
	}
	return records, len(scanned), nil
}

// readBatch is the live half's read transaction: seq in (liveRead, head] in
// order, one bounded page, with a table feed's registration-label filter and
// a current-lifetime check — a target that moved under the session ends it
// (a drop, or a drop-and-recreate: a same-named successor is a different
// feed) instead of silently narrowing.
func (sess *listenSession) readBatch(ctx context.Context) ([]loggedChange, int64, error) {
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, sess.fillErr(err)
	}
	defer tx.Rollback()
	if sess.feed != nil {
		current, ferr := changeFeedOf(ctx, tx, sess.nsName, sess.feed.table)
		if ferr != nil {
			return nil, 0, sess.fillErr(ferr)
		}
		if current.nsgen != sess.feed.nsgen || current.dropGen != sess.feed.dropGen {
			return nil, 0, ErrListenLifetimeEnded
		}
	}
	sess.mu.Lock()
	from := sess.liveRead
	sess.mu.Unlock()
	query, args := changePageSQL(from, 0, MaxChangesPageLimit, sess.feed)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, sess.fillErr(err)
	}
	scanned, err := scanChangePage(rows)
	rows.Close()
	if err != nil {
		return nil, 0, sess.fillErr(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, sess.fillErr(err)
	}
	return scanned, from, nil
}

// mint writes one transaction of cursor mints plus opportunistic retention
// pruning — the shared tail of both session halves, matching ChangesSince's
// mint-then-prune shape so every replay path refreshes chains identically.
// The next-page token matters to the replay half; the live half's records
// carry their own cursors and ignores it.
func (sess *listenSession) mint(ctx context.Context, now time.Time, admitted []loggedChange, resume int64) ([]ChangeRecord, Cursor, error) {
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	records, next, err := mintChangeCursors(ctx, tx, now, admitted, resume, "", sess.table, sess.chain)
	if err != nil {
		return nil, "", err
	}
	if err := pruneChanges(ctx, tx, now, sess.s.changeRetention); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return records, next, nil
}

// fillErr maps a fill failure onto the session's close causes: a namespace
// no longer served by this store instance — dropped and evicted, replaced by
// a recreated successor — is the teaching lifetime end, never a successor's
// records through the predecessor's session; everything else is an engine
// failure the caller sees verbatim.
func (sess *listenSession) fillErr(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrListenLifetimeEnded
	}
	if sess.s.nsEvicted(sess.nsName, sess.n) {
		return ErrListenLifetimeEnded
	}
	return fmt.Errorf("listen fill %s: %w", sess.nsName, err)
}

// admit is the live per-event authorization gate (§6.2): it runs BEFORE a
// record is queued (live) or exposed (replay), against the record's own
// target table. visible reports whether the record may reach the caller;
// revoked reports that the authorization itself ended the stream — ok=false
// is a teaching close, never a silent skip. The engine stays grant-blind: it
// filters by the returned scope and the record's Owner label, and — on a
// table feed — compares the returned incarnation with the record's Lifetime,
// so a drop-and-recreate between the re-resolver's answer and admission
// cannot carry a stale unscoped decision onto the successor's records.
// Namespace-wide feeds make no per-record lifetime comparison (their replay
// spans table lifetimes by design, §9.3); the namespace's own lifetime is
// the session binding fillErr enforces.
func (sess *listenSession) admit(rec ChangeRecord) (visible, revoked bool) {
	if sess.liveAuthz == nil {
		return true, false
	}
	scope, inc, ok := sess.liveAuthz(rec.Table)
	if !ok {
		return false, true
	}
	if scope != nil && (scope.Empty || scope.Owner != rec.Owner) {
		return false, false
	}
	if sess.table != "" && inc != (Incarnation{}) {
		if (Lifetime{NsGen: inc.NsGen, Table: inc.Table, DropGen: inc.DropGen}) != rec.Lifetime {
			return false, false
		}
	}
	return true, false
}

// drain is the session's delivery half: it waits until the caller has
// drained the replay (Next reported done — the §6.2 gating), then hands the
// queued records to notify one at a time, in seq order. It holds no lock
// across notify: a slow client delays only itself and the queue behind it,
// never a writer, never the filler.
func (sess *listenSession) drain() {
	defer sess.pumps.Done()
	defer sess.recoverPump("drain")
	for {
		sess.mu.Lock()
		for !sess.dead && (!sess.replayDone || len(sess.queue) == 0) {
			sess.cond.Wait()
		}
		if sess.dead {
			sess.mu.Unlock()
			return
		}
		rec := sess.queue[0]
		sess.queue = sess.queue[1:]
		sess.mu.Unlock()

		sess.notify(rec)
	}
}

// recoverPump keeps a panicking session callback from taking the process
// down — deliverCommit's write-path rule, applied to the session's own
// goroutines. The session ends with the recovered panic as its cause: the
// stream is already unreliable from the caller's perspective, and the
// durable log recovers everything queued behind it.
func (sess *listenSession) recoverPump(half string) {
	if r := recover(); r != nil {
		slog.Error("listen pump panicked; ending session",
			"namespace", sess.nsName, "table", sess.table, "half", half, "panic", r)
		sess.end(fmt.Errorf("listen %s pump panicked: %v", half, r))
	}
}

// next pages the replay half: records in (position, boundary] in cursor
// order, one bounded page per call, each admitted record carrying a fresh
// cursor on the registration chain. The page carrying the final replay
// records returns done=false; the following call returns done=true with no
// records — the registration boundary — after which notify delivers live
// records (an empty replay range reports done on its first call). A session
// ended under the page — overflow, revocation found by the admission filter,
// a feed target that moved — reports done too: the closed callback has
// already carried the cause to the caller, and Next never contradicts it.
func (sess *listenSession) next(ctx context.Context) ([]ChangeRecord, Cursor, bool, error) {
	sess.mu.Lock()
	dead, exhausted := sess.dead, sess.replayExhausted
	sess.mu.Unlock()
	if dead || exhausted {
		sess.markReplayDone()
		return nil, sess.cursor(), true, nil
	}

	// The page read: (position, boundary] under the registration labels —
	// the labels and the boundary are registration-fixed, so a drop or
	// recreate committing mid-replay can neither narrow this range nor mix
	// a successor's records into it.
	now := time.Now()
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", false, err
	}
	query, args := changePageSQL(sess.position, sess.boundary, MaxChangesPageLimit, sess.feed)
	rows, qerr := tx.QueryContext(ctx, query, args...)
	var scanned []loggedChange
	if qerr == nil {
		scanned, qerr = scanChangePage(rows)
		rows.Close()
	}
	if qerr == nil {
		qerr = tx.Commit()
	} else {
		tx.Rollback()
	}
	if qerr != nil {
		return nil, "", false, qerr
	}

	// Admit outside the transaction, like fillBatch: revocation ends the
	// session (closed carries the cause), invisibility drops the record from
	// the page. Records admitted before a revocation in the same page are
	// still exposed; the revoked one never is.
	var admitted []loggedChange
	for _, lc := range scanned {
		vis, revoked := sess.admit(lc.rec)
		if revoked {
			sess.end(ErrListenRevoked)
			break
		}
		if vis {
			admitted = append(admitted, lc)
		}
	}

	records, next, merr := sess.mint(ctx, now, admitted, sess.position)
	if merr != nil {
		return nil, "", false, merr
	}

	sess.mu.Lock()
	sess.replayExhausted = len(scanned) < MaxChangesPageLimit
	if len(scanned) > 0 {
		sess.position = scanned[len(scanned)-1].seq
	}
	sess.nextCursor = next
	dead = sess.dead
	sess.mu.Unlock()
	// A session ended under the page drops it: the closed callback has
	// already carried the cause, and records must not follow the terminal.
	if dead {
		return nil, next, true, nil
	}
	// An empty page IS the boundary — the caller is already at the
	// registration point and notify takes over from here.
	if len(records) == 0 {
		sess.markReplayDone()
		return nil, next, true, nil
	}
	return records, next, false, nil
}

// markReplayDone releases the drainer: the caller has drained the replay
// (or the session ended and the distinction no longer matters), so queued
// live records may flow.
func (sess *listenSession) markReplayDone() {
	sess.mu.Lock()
	sess.replayDone = true
	sess.cond.Broadcast()
	sess.mu.Unlock()
}

// cursor returns the replay's latest next-page cursor — what a caller that
// stopped paging mid-replay would resume from.
func (sess *listenSession) cursor() Cursor {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.nextCursor
}

func (sess *listenSession) isDead() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.dead
}

// end terminates the session. A non-nil cause is an ENGINE-initiated end:
// closed fires — once, on this goroutine — with it. A nil cause is the
// caller's cancel: the caller already knows, and closed never fires for it
// (§6.2). Idempotent; the first end wins.
func (sess *listenSession) end(cause error) {
	sess.mu.Lock()
	if sess.dead {
		sess.mu.Unlock()
		return
	}
	sess.dead = true
	sess.cond.Broadcast()
	sess.mu.Unlock()
	if cause == nil {
		return
	}
	sess.closedOnce.Do(func() {
		if sess.closedFn != nil {
			sess.closedFn(cause)
		}
	})
}

// cancel is the returned teardown: it unregisters the listener, ends the
// session without a closed callback, and waits for both pumps — so once it
// returns, no notify or closed call can still be running (the use-after-free
// rule from notify.go's listener contract; the SSE handler returns only
// after this).
func (sess *listenSession) cancel() {
	sess.cancelOnce.Do(func() {
		sess.s.untrackSession(sess)
		sess.unregister()
		sess.end(nil)
		sess.pumps.Wait()
	})
}
