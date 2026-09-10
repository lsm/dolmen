package store

import (
	"context"
	"database/sql"
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

// listenPollInterval is the fill loop's poll fallback: the commit registry
// only wakes for commits through THIS store instance, while the persisted
// concurrency guards explicitly support another Store — another dolmen
// process — writing the same namespace database. Its commits update the
// durable log but fire no wake here, so without a periodic re-read a live
// session could sleep past them indefinitely (§9.3: the log, never the
// wake, is the delivery guarantee). The interval matches wait_for's poll
// tick (slice 5d): the same fallback, applied to a stream instead of a
// long-poll.
const listenPollInterval = 250 * time.Millisecond

// nsPruneInterval coalesces the read-path retention prune across every
// listener on a namespace (pruneDue): each session's poll fallback wakes on
// its own ticker, and a prune per tick would run three DELETE statements on
// the single-connection write pool 4×listener-count times per second —
// maintenance work queued ahead of real mutations, linearly in subscriber
// count. One prune per interval per namespace bounds it to the rate a
// single wait_for poller already generates.
const nsPruneInterval = listenPollInterval

// pruneDue reports whether this read should carry the namespace's retention
// prune, admitting one carrier per nsPruneInterval. Callers hold no store
// locks when asking — readBatch calls this BEFORE opening its transaction —
// so notifyMu (the listener-side lock) is safe to take here.
func (s *Store) pruneDue(ns string, now time.Time) bool {
	if s.changeRetention <= 0 {
		// Retention disabled: pruning is a guaranteed no-op, and taking the
		// rw connection (and SQLite's writer lock) for it every interval
		// would still contend with real writers for nothing.
		return false
	}
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.pruneNext == nil {
		s.pruneNext = map[string]time.Time{}
	}
	if now.Before(s.pruneNext[ns]) {
		return false
	}
	s.pruneNext[ns] = now.Add(nsPruneInterval)
	return true
}
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

	// Registration-fixed replay state — immutable once Listen returns,
	// except the chain, which rotates at its retention cap (chainFor).
	position    int64        // resume position P: the client's `from`, resolved
	boundary    int64        // registration boundary R: replay is (P, R], live is > R
	outstanding int64        // loss-check baseline: rows the log retained in (P, R] AT REGISTRATION — holes included; the pages' promise
	chain       *cursorChain // the page chain every token the session mints rides; guarded by mu once live
	feed        *changeFeed  // nil on the namespace feed; the table feed's labels at registration

	mu              sync.Mutex
	cond            *sync.Cond
	queue           []loggedChange // bounded by listenQueueBound, in seq order; cursors mint at delivery
	liveRead        int64          // last live position read from the log, visible or not
	woken           bool
	replayDone      bool // Next has drained the replay: the drainer may deliver
	replayExhausted bool
	nextCursor      Cursor
	dead            bool
	pendingClose      error // a terminal parked until its serialization point (below): fired from a pump's quiescent exit
	pendingDrainClose error // a queue-owned terminal (a revocation with an admitted prefix): ONLY the drainer may fire it, after the prefix drains — the fill's own flush must not race it
	replayActive    bool  // a replay page is in flight: a parked close must not cut past it
	notifyActive    bool  // a delivery is in flight (mint + notify): same rule

	closedOnce sync.Once
	cancelOnce sync.Once
	pumps      sync.WaitGroup
	stop       chan struct{}    // closed once, at end(): the poll pump's immediate stop signal
	ctx        context.Context // the session's cancellation scope: every pump database operation runs on it, so ending the session aborts in-flight SQLite waits (busy timeout included) instead of making cancel wait them out
	ctxCancel  context.CancelFunc
	unregister func()
	delivered  atomic.Int64 // live deliveries, for periodic retention pruning
}

// Listen implements the engine-declared notification capability (§6.2, §9.3):
// the listener registration and the replay boundary are fixed as ONE
// coordinated operation, the replay pages out through ChangeReplay.Next, and
// once the caller drains it the session pushes live records through notify.
// closed fires at most once, only when the ENGINE ends the session
// (ErrListenOverflow, ErrListenRevoked, ErrListenLifetimeEnded, or an engine
// failure) — never for a caller-initiated cancel. The returned cancel is
// idempotent and returns only once the session is quiescent: after it
// returns, no callback will ever run again (notify.go's teardown rule). The
// callbacks carry the same kind of rule onCommit's registry documents: cancel
// must not be called from inside notify or closed — the wait would sit on the
// calling goroutine's own pump (see cancel).
//
// TODO(9d): while auth is off the SSE handler passes a nil liveAuthz (no
// per-event filter); the admission rules below are live the moment a slice
func (s *Store) Listen(ctx context.Context, nsName, table string, from Cursor, nsGen [16]byte, liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool), notify func(ChangeRecord), closed func(cause error)) (*ChangeReplay, func(), error) {
	if notify == nil {
		return nil, nil, invalidf("listen: notify callback is required")
	}
	// nsCtx, not ns: registration may queue behind s.mu — a DropNamespace
	// draining its in-flight connections holds it, and that drain is
	// explicitly unbounded — so the caller's context must bound both the
	// mutex wait and a first-open initialization (the same rule
	// ChangesSince follows).
	n, err := s.nsCtx(ctx, nsName)
	if err != nil {
		return nil, nil, err
	}
	sess := &listenSession{
		s: s, n: n, nsName: nsName, table: table, nsGen: nsGen,
		liveAuthz: liveAuthz, notify: notify, closedFn: closed,
	}
	sess.cond = sync.NewCond(&sess.mu)
	sess.stop = make(chan struct{})
	// The pumps outlive the registration call; their cancellation scope is
	// the session itself, derived from the caller's context (the handler
	// passes one that ends with the client OR the server's shutdown) and
	// canceled at end — so a session being torn down never waits out an
	// in-flight SQLite busy timeout inside cancel.
	sess.ctx, sess.ctxCancel = context.WithCancel(ctx)

	// Atomic register-and-replay, ordering half one: the listener joins BOTH
	// registries — the commit wake (onCommit) and the lifecycle wake
	// (listenSessions) — BEFORE the boundary transaction runs (see the
	// session comment for the exactly-once argument). Lifecycle registration
	// cannot wait for the pumps: a drop committing between the boundary
	// transaction and a later track would snapshot a map without this
	// session, and a dropped target mints no further commit to wake it —
	// the session would sleep forever. Registered here, a drop either fails
	// the boundary transaction outright or lands a wake the first fill
	// observes. Failure past this point must leave both registries as they
	// were found.
	sess.unregister = s.onCommit(nsName, sess.wake)
	s.trackSession(sess)
	committed := false
	defer func() {
		if !committed {
			sess.ctxCancel()
			sess.unregister()
			s.untrackSession(sess)
		}
	}()

	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	// now is captured AFTER the transaction is held: BeginTx can queue
	// behind the namespace's single write connection for as long as its
	// current holder takes, and a timestamp from before that wait would
	// mint the standing cursor with issuance and chain start already stale
	// — or refresh a presented token against a time from before it truly
	// expired — leaving the returned resume cursor dead on arrival.
	now := time.Now()

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
		// The presented token is RETAINED, not re-minted: it resolves at P,
		// and resolve just refreshed its deadline, so it resumes exactly as
		// a fresh one would (the empty-page echo's rule, §9.3).
		sess.nextCursor = from
	}

	// Ordering half two: the boundary. The immediate write lock this
	// transaction holds means no writer can interleave between the head read
	// and the commit — R is the head of a serial-observability point (§0.6),
	// and everything after it is live-only.
	if sess.boundary, err = changeHead(ctx, tx); err != nil {
		return nil, nil, err
	}
	// The loss-check baseline, in the same snapshot: the feed's rows the
	// log ACTUALLY retains in (P, R] — not the span arithmetic, because
	// pruning deletes by age and clock-stepped stamps can leave holes a
	// begin registration legitimately replays around (changeBegin selects
	// the first retained record, whatever sits missing beside it), and not
	// the whole namespace's, because a table feed's promise is only its own
	// records. Only rows removed AFTER this count are the session's to
	// lose.
	// TODO(9d): the count covers every FEED row, including rows a scoped
	// viewer can never receive — a hidden aged row deleted mid-session
	// currently reads as loss even when the visible backlog is intact
	// (§9.3 says foreign traffic must never evict a scoped reader).
	// Filtering the count needs a side-effect-free view of the current
	// scope; re-invoking liveAuthz is NOT it — the admission callback is
	// per-event by contract (§6.2), and extra invocations from the counting
	// path perturb re-resolvers that sequence or audit on calls. The 9d
	// scope-resolver contract should expose the predicate; until then, with
	// auth off, liveAuthz is nil everywhere but tests.
	if sess.position < sess.boundary {
		cq, cargs := changeCountSQL(sess.position, sess.boundary, sess.feed)
		if err = tx.QueryRowContext(ctx, cq, cargs...).Scan(&sess.outstanding); err != nil {
			return nil, nil, err
		}
	}
	// The live read starts exactly past the boundary: everything at or
	// before R belongs to the replay half, and the two disjoint ranges are
	// the exactly-once guarantee itself. Set before the pumps start, so no
	// fill can race it.
	sess.liveRead = sess.boundary
	// The standing resume cursor is fixed HERE, at registration, so a
	// session the pumps end before its first Next — an overflow of its own
	// traffic, a dropped feed target — still hands back a boundary the
	// caller can resume from EXACTLY where registration stood: a terminal
	// reporting an empty cursor would send the reconnecting client to the
	// current head and skip every undelivered commit after registration.
	// Bare and begin starts mint one at the resume position (the
	// undelivered backlog stays behind the cursor); a presented token was
	// retained above.
	if sess.nextCursor == "" {
		var terr error
		if sess.nextCursor, terr = mintCursorToken(ctx, tx, now, sess.position, table, sess.chain); terr != nil {
			return nil, nil, terr
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	committed = true

	sess.pumps.Add(3)
	go sess.fill()
	go sess.drain()
	go sess.pollWake()
	return &ChangeReplay{Next: sess.next, Resume: sess.cursor}, sess.cancel, nil
}

// trackSession registers the session for LIFECYCLE wakes — a different event
// from the commit wake onCommit serves: "new commits exist" vs "the feed's
// world changed; look again". A drop leaves no further commits to wake a
// session with, yet a dropped namespace's streams must end at the drop
// (notify.go's binding note), so DropNamespace and DropTable nudge every
// session of the namespace and each session's next fill decides its own
// fate. Listen tracks BEFORE its boundary transaction runs, so a drop
// racing the registration still lands its nudge. notifyMu — the same lock
// as the commit registry — keeps a lifecycle wake from interleaving with
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

// pollWake is the fill loop's durability fallback (listenPollInterval): the
// ticker pokes the session awake on a timer, in addition to the commit
// registry's wakes, so the durable log is re-read even when nothing in THIS
// process ever commits — another store instance writing the same namespace
// leaves no wake behind. The poke is the same advisory flag a commit's wake
// raises; the fill re-derives everything from the log, so an idle namespace
// pays one empty read per tick and nothing else. The session's stop channel
// ends it IMMEDIATELY — cancel's pumps.Wait must not sit out a tick for it,
// or every teardown (a subscriber disconnecting, above all) would carry the
// poll interval as latency.
func (sess *listenSession) pollWake() {
	defer sess.pumps.Done()
	t := time.NewTicker(listenPollInterval)
	defer t.Stop()
	for {
		select {
		case <-sess.stop:
			return
		case <-t.C:
			sess.wake("", ChangeRange{})
		}
	}
}
// protectQueue keeps the queued rows durably protected while a slow
// subscriber blocks the drainer. The prune's deletion guard reads TOKEN
// ROWS — an in-memory rotation protects nothing — so once the current chain
// approaches its cap (one poll interval of margin: no prune can observe an
// expired chain before the next fill refreshes the protection), the chain
// rotates and ONE token is persisted at the queue's head: the guard deletes
// only rows at or below the oldest chain origin, so a chain rooted at the
// head pins every queued position. The drainer cannot carry this — a
// blocked drainer mints nothing — which is why the fill does, before the
// read that may carry the prune. Best-effort on failure: the next fill
// retries inside the margin, and only the tail past 2R is at risk.
func (sess *listenSession) protectQueue(from int64) {
	if sess.s.changeRetention <= 0 {
		return
	}
	sess.mu.Lock()
	var head int64
	if len(sess.queue) > 0 {
		head = sess.queue[0].seq
	}
	chain := sess.chain
	sess.mu.Unlock()
	if head == 0 {
		return // nothing queued: the next delivery's mint protects itself
	}
	rms := int64(sess.s.changeRetention / time.Millisecond)
	now := time.Now()
	if now.UnixMilli() < chain.Start+2*rms-int64(listenPollInterval/time.Millisecond) {
		return // comfortably inside the cap
	}
	// Rotate HERE, in the margin, not through chainFor: chainFor keeps the
	// existing chain until the cap is actually reached, so a token minted
	// "early" would still ride the nearly-expired chain_start and die with
	// it — another pruner's window between our cap and next poll. The
	// replacement is created and persisted while the old chain is still
	// valid, rooted under the same floors chainFor would apply.
	sess.mu.Lock()
	origin := from
	if !sess.replayExhausted && sess.position < origin {
		origin = sess.position
	}
	// Root STRICTLY BELOW the queued head: pruneChanges deletes
	// seq <= MIN(chain_origin) — inclusive — so a chain rooted AT the head
	// leaves the first queued record deletable, and its loss would fail the
	// drainer's existence check and kill the buffered prefix's session.
	if head-1 < origin {
		origin = head - 1
	}
	replacement := newCursorChain(now, origin)
	chain = replacement
	sess.mu.Unlock()
	ctx := sess.ctx
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("listen queue protection: begin", "namespace", sess.nsName, "err", err)
		return
	}
	defer tx.Rollback()
	if _, err := mintCursorToken(ctx, tx, now, head, sess.table, chain); err != nil {
		slog.Error("listen queue protection: mint", "namespace", sess.nsName, "err", err)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Error("listen queue protection: commit", "namespace", sess.nsName, "err", err)
		return
	}
	// Publish the replacement ONLY now that its protective token is
	// durable: a transient begin/mint/commit failure left published would
	// point the session at a fresh chain with no token — every later poll
	// would see its young Start and skip protection, and once the old
	// chain expired a pruner could delete the queued rows. On failure the
	// old chain stays current and the next fill retries inside the margin.
	sess.mu.Lock()
	sess.chain = replacement
	sess.mu.Unlock()
}

// isClosing reports whether the session must take no further reads: it is
// dead, or a terminal is parked (a revocation found mid-batch). The first
// ok=false is terminal by contract, so no later fill may admit again — not
// even when access is re-granted a beat later and liveAuthz would pass; the
// parked close still lets the already-admitted prefix drain, and the replay
// half's own pages end the session through their revocation path.
func (sess *listenSession) isClosing() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.dead || sess.pendingClose != nil || sess.pendingDrainClose != nil
}

func (sess *listenSession) fill() {
	defer sess.pumps.Done()
	defer sess.flushParkedClose()
	defer sess.recoverPump("fill")
	for {
		sess.mu.Lock()
		for !sess.dead && !sess.woken {
			sess.cond.Wait()
		}
		closing := sess.dead || sess.pendingClose != nil || sess.pendingDrainClose != nil // isClosing's check, inline under the held lock
		sess.woken = false
		sess.mu.Unlock()
		if closing {
			return
		}

		for {
			if sess.isClosing() {
				return
			}
			batch, read, ferr := sess.fillBatch()
			if ferr != nil {
				sess.end(ferr)
				return
			}
			if len(batch) > 0 {
				sess.mu.Lock()
				if sess.dead { // ended while the batch was in flight
					sess.mu.Unlock()
					return
				}
				sess.queue = append(sess.queue, batch...)
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
// inside the namespace's write lock could deadlock against it). The admitted
// records queue WITHOUT cursors — tokens are minted at delivery (drain).
// liveRead advances past every record read, visible or not: an invisible
// record is delivered never, but its position is consumed exactly once.
func (sess *listenSession) fillBatch() (admitted []loggedChange, read int, err error) {
	ctx := sess.ctx // the session's scope: cancel aborts in-flight database work, not just future reads
	sess.mu.Lock()
	from := sess.liveRead
	sess.mu.Unlock()
	// Refresh the queue's durable protection BEFORE the read: the read may
	// carry the prune that would otherwise delete the queued rows out from
	// under the blocked drainer (protectQueue).
	sess.protectQueue(from)
	scanned, _, rerr := sess.readBatch(ctx)
	if rerr != nil {
		return nil, 0, rerr
	}
	sess.mu.Lock()
	if len(scanned) > 0 {
		sess.liveRead = scanned[len(scanned)-1].seq
	}
	sess.mu.Unlock()

	revoked := false
	for _, lc := range scanned {
		vis, rev := sess.admit(lc.rec)
		if rev {
			revoked = true
			break
		}
		if vis {
			admitted = append(admitted, lc)
		}
	}
	if revoked {
		// The admitted prefix is DELIVERED before the close: liveRead has
		// consumed those positions, and a revocation takes effect at the
		// next event, not retroactively. Queue it and arm a pending close —
		// the drainer mints and delivers the prefix, then fires the close
		// from its quiescent exit, so closed never precedes a record it
		// admitted (§6.2's exposure rule). An empty prefix closes through
		// the same path. A cancellation that already ended the session
		// (dead, under this lock) wins: no terminal is parked for a
		// caller-initiated cancel (§6.2 — closed never fires for it), and
		// the prefix dies with the session.
		sess.mu.Lock()
		if sess.dead {
			sess.mu.Unlock()
			return nil, 0, nil
		}
		sess.queue = append(sess.queue, admitted...)
		// The DRAIN owns this close: the fill's deferred flushParkedClose
		// would fire it from the fill's exit while the drainer still holds
		// the prefix — records after the terminal, a wedged drainer (the
		// cause consumed), exposure violations if the replay is still
		// paging. The drainer picks it up when the queue empties, moves it
		// through end(), and fires it from its own quiescent exit.
		sess.pendingDrainClose = ErrListenRevoked
		sess.cond.Broadcast()
		sess.mu.Unlock()
		return nil, 0, nil
	}
	// Nothing is minted on the fill path: live records carry their cursors
	// minted at DELIVERY (drain), on the chain current at that moment — a
	// queued record never holds a token that a rotation or its prune could
	// orphan while it waits behind a slow subscriber (§9.3's chain cap).
	return admitted, len(scanned), nil
}

// readBatch is the live half's read transaction: seq in (liveRead, head] in
// order, one bounded page, with a table feed's registration-label filter and
// a current-lifetime check — a target that moved under the session ends it
// (a drop, or a drop-and-recreate: a same-named successor is a different
func (sess *listenSession) readBatch(ctx context.Context) ([]loggedChange, int64, error) {
	// The idle poll reads READ-ONLY: the rw pool's DSN takes SQLite's
	// writer lock for every transaction (_txlock=immediate), and one
	// write-lock transaction per listener per 250ms would queue ahead of
	// real mutations linearly in subscriber count — and contend with other
	// processes' writers. Only the prune-carrying read (one per interval
	// per namespace) rides rw; every other read runs on the read-only pool,
	// which sees every committed change in WAL mode.
	prune := sess.s.pruneDue(sess.nsName, time.Now())
	var tx *sql.Tx
	var err error
	if prune {
		tx, err = sess.n.rw.BeginTx(ctx, nil)
	} else {
		tx, err = sess.n.ro.BeginTx(ctx, nil)
	}
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
	query, args := changePageSQL(from, nil, MaxChangesPageLimit, sess.feed)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, sess.fillErr(err)
	}
	scanned, err := scanChangePage(rows)
	rows.Close()
	if err != nil {
		return nil, 0, sess.fillErr(err)
	}
	// Retention moves with the reads — including reads that deliver
	// nothing: a subscriber whose entire traffic the authorization filters
	// out never mints (the drainer's per-delivery prune never runs), yet
	// its fills keep moving the log head, and without a read-path prune
	// expired tokens and age-eligible records would accumulate without
	// bound (§9.3). The work is COALESCED per namespace though: every live
	// session polls, and a prune per listener tick would queue maintenance
	// write transactions on the single rw connection linearly in subscriber
	// count — pruneDue admits one carrier per interval.
	if prune {
		// The just-scanned rows are deletion candidates to this very prune
		// once their chain has expired (an empty queue meant protectQueue
		// had nothing to root — a cross-instance commit landing between
		// polls, a stalled filler). Mint their protection FIRST, atomically
		// with the prune in this transaction: a token at the batch's start
		// roots the guard below the scanned range, and the minted chain's
		// fresh start survives the chain expiry delete. The drainer's
		// delivery mints refresh it thereafter.
		if len(scanned) > 0 {
			if _, err := mintCursorToken(ctx, tx, time.Now(), from, sess.table, sess.chainFor(time.Now(), from)); err != nil {
				return nil, 0, sess.fillErr(err)
			}
		}
		if err := pruneChanges(ctx, tx, time.Now(), sess.s.changeRetention); err != nil {
			return nil, 0, sess.fillErr(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, sess.fillErr(err)
	}
	// The SCAN COUNT, not the position, is the second return: a caller
	// comparing it against the page bound must see rows scanned, never the
	// liveRead position (a deep history puts that far past the bound, and
	// every empty read would look like a full page — an unbounded spin of
	// read transactions over an idle namespace). The current caller
	// recomputes len(scanned) itself; this keeps the contract honest.
	return scanned, int64(len(scanned)), nil
}

// mint writes one transaction of cursor mints plus opportunistic retention
// pruning — the shared tail of both session halves, matching ChangesSince's
// mint-then-prune shape so every replay path refreshes chains identically.
// The next-page token matters to the replay half; the live half's records
func (sess *listenSession) mint(ctx context.Context, admitted []loggedChange, resume int64, scannedLast int64, scannedCount int) ([]ChangeRecord, Cursor, error) {
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	// now is captured AFTER the transaction is held — the same rule as
	// registration: BeginTx can queue behind the namespace's single write
	// connection, and stamps from before the wait would hand the caller
	// cursors whose issuance is already expired (and evaluate chain
	// rotation against a stale, pre-cap time).
	now := time.Now()
	// The loss check REVALIDATED here, under the mint's own write lock: the
	// page released its read snapshot before admission ran, and the
	// authorization callbacks are caller code that can take arbitrarily
	// long — in that gap another reader's prune may have deleted the
	// expired chain and the scanned, age-eligible rows. Minting tokens for
	// deleted positions would hand the caller cached records whose cursors
	// resolve over a gutted log. The recheck is BOUNDED to what the page
	// consumed — the full outstanding tail is verified before the scan (the
	// page's entry check), not counted again per mint.
	if scannedCount > 0 {
		cq, cargs := changeCountSQL(resume, scannedLast, sess.feed)
		var kept int64
		if err := tx.QueryRowContext(ctx, cq, cargs...).Scan(&kept); err != nil {
			return nil, "", err
		}
		if kept != int64(scannedCount) {
			return nil, "", fmt.Errorf("listen replay: %w", ErrCursorExpired)
		}
	}
	records, next, err := mintChangeCursors(ctx, tx, now, admitted, resume, "", sess.table, sess.chainFor(now, resume))
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

// chainFor returns the chain this mint rides, rotating it once the current
// one is past its absolute cap (§9.3): a token minted on a capped chain is
// born expired — resolve rejects anything past chain_start + 2R, and this
// very transaction's prune would delete it — so a stream held longer than
// 2R would hand its client dead cursors. A fresh chain roots at the current
// position, exactly the chain a client resubscribing from here starts
// itself: the old backlog the cap exists to bound stays bounded, and every
// delivered cursor stays resolvable. Retention 0 has no caps and never
// rotates.
//
// While the replay is still draining, a rotation's origin floors at the
// replay's outstanding position: the live half mints first (its fills run
// during replay), and a chain rooted at the live position would let the
// same transaction's prune delete age-eligible records the replay has not
// yet paged — silently shortening the gap-free resume the chain exists to
// protect. Once the replay is exhausted the floor lifts; nothing below the
// live position remains to protect.
func (sess *listenSession) chainFor(now time.Time, resume int64) *cursorChain {
	if sess.s.changeRetention <= 0 {
		return sess.chain
	}
	rms := int64(sess.s.changeRetention / time.Millisecond)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if now.UnixMilli() < sess.chain.Start+2*rms {
		return sess.chain
	}
	origin := resume
	if !sess.replayExhausted && sess.position < origin {
		origin = sess.position
	}
	// The queue's head floors the rotation too: queued records carry no
	// tokens yet (delivery mints them), so the chain is the ONLY durable
	// protection their log rows have — a subscriber blocked past 2R must
	// not have its buffered backlog pruned out from under the queue, or the
	// delivered cursors would resolve over a log that no longer holds the
	// records between them (§9.3's gap-free reconnect). The fill calls
	// chainFor on every wake, so the protecting chain stays rotated — and
	// fresh — for as long as anything sits queued.
	if len(sess.queue) > 0 && sess.queue[0].seq < origin {
		origin = sess.queue[0].seq
	}
	sess.chain = newCursorChain(now, origin)
	return sess.chain
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
// the session binding the live half enforces.
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

// mint writes one transaction of cursor mints plus opportunistic retention
// pruning — matching ChangesSince's mint-then-prune shape so every replay
// path refreshes chains identically. The next-page token is the replay's
func (sess *listenSession) drain() {
	defer sess.pumps.Done()
	defer sess.flushParkedClose()
	defer sess.recoverPump("drain")
	for {
		sess.mu.Lock()
		// Live delivery is gated on the boundary call having COMPLETED —
		// not merely on replayDone being set — and on no page still being
		// in flight: replayDone flips inside next()'s locked publish, so
		// the caller has its final result by the time this goroutine can
		// proceed, and replayActive holds off delivery while any later
		// page (the caller keeps paging past done) is being decided.
		for !sess.dead && (!sess.replayDone || sess.replayActive || (len(sess.queue) == 0 && sess.pendingClose == nil && sess.pendingDrainClose == nil)) {
			sess.cond.Wait()
		}
		if sess.dead {
			sess.mu.Unlock()
			return
		}
		if len(sess.queue) == 0 {
			cause := sess.pendingClose
			if sess.pendingDrainClose != nil {
				// The queue-owned revocation close: the prefix has fully
				// drained (the queue is empty), so this goroutine — the
				// only legal firing point — moves it through end().
				cause = sess.pendingDrainClose
				sess.pendingDrainClose = nil
			}
			sess.mu.Unlock()
			// end() is idempotent-safe here: the cause is already parked, so
			// this only flips dead and lets the deferred flushParkedClose
			// fire it from this goroutine's quiescent exit — after any
			// in-flight notify returned, and waiting out an active replay
			// page.
			sess.end(cause)
			return
		}
		lc := sess.queue[0]
		sess.queue = sess.queue[1:]
		sess.notifyActive = true
		sess.mu.Unlock()

		// The record's cursor is minted HERE, at delivery: the token rides
		// the chain current at this moment, so a queued record never holds
		// a cursor a rotation could orphan while it waited behind a slow
		// subscriber (§9.3's chain cap), and every delivered cursor is as
		// fresh as its delivery. notifyActive covers the whole
		// mint-and-deliver span — cleared by the defer so even a panicking
		// notify cannot strand the flag and wedge every parked close (and
		// cancel's pumps.Wait) behind it.
		func() {
			defer func() {
				sess.mu.Lock()
				sess.notifyActive = false
				sess.cond.Broadcast()
				sess.mu.Unlock()
			}()
			tok, merr := sess.mintOne(sess.ctx, lc.seq)
			if merr != nil {
				sess.end(sess.fillErr(merr))
				return
			}
			lc.rec.Cursor = tok
			sess.notify(lc.rec)
		}()
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
// ended under the page — overflow, a feed target that moved, or a revocation
// this page's admission found — reports done too, the page OMITTED on
// revocation: the closed callback has already fired, and nothing may be
// exposed after it (§6.2); the omitted records re-deliver on the caller's
// reconnect from its last cursor.
// next marks the page in flight and defers to page: a terminal parked while
// the page runs (an overflow, a dropped feed target — end() from either
// pump) waits for the page instead of cutting past it, so closed never
// precedes records this call is about to expose (§6.2's exposure rule). A
// page whose session ended before the flag clears — the end parked or fired
// while the page was building — is omitted: nothing follows the terminal.
// An end landing after the clear fires concurrently with the return, a seam
// Go cannot close across a function boundary; the API layer's terminal
// discipline closes it client-side (the handler's page frames are ordered
func (sess *listenSession) next(ctx context.Context) ([]ChangeRecord, Cursor, bool, error) {
	sess.mu.Lock()
	resume := sess.nextCursor // the PRE-page boundary: what an omitted page must hand back
	// Mark the page IN FLIGHT for its whole span — read, authorization,
	// mint: a parked close (overflow, a dropped target, shutdown) must not
	// fire from a pump's quiescent exit while this page can still expose
	// records (§6.2's exposure rule; flushParkedClose waits on the flag),
	// and the drainer must not deliver past the boundary while the final
	// page is still being decided. The clear rides a DEFER so a panicking
	// liveAuthz cannot strand the flag and wedge every parked close (and
	// cancel's pump wait) behind it.
	sess.replayActive = true
	sess.mu.Unlock()
	records, next, done, progress, err := func() (a []ChangeRecord, b Cursor, c bool, d pageProgress, e error) {
		defer func() {
			sess.mu.Lock()
			sess.replayActive = false
			sess.cond.Broadcast()
			sess.mu.Unlock()
		}()
		return sess.page(ctx)
	}()
	sess.mu.Lock()
	// done with no error — from ANY path, including the exhausted-entry
	// boundary call that carries zero progress — means the replay is over:
	// release the drainer HERE, inside the caller's own final word (never
	// mid-page), before the branches below decide what to publish.
	if done && err == nil {
		sess.replayDone = true
		sess.cond.Broadcast()
	}
	// Publish or drop the page's progress HERE, under one lock, after the
	// omission decision is final: publishing inside page left a window
	// where a concurrent Resume() observed the post-page cursor and the
	// omission then rolled it back — a returned cursor cannot be retracted.
	if sess.dead && len(records) > 0 {
		// The page is omitted: nothing advances, and the standing cursor
		// stays at the PRE-page boundary — the omitted page's own cursor
		// points past records the caller never receives, and Resume must
		// never teach that position.
		sess.replayDone = true // the session is over; release any drainer
		sess.cond.Broadcast()
		sess.mu.Unlock()
		return nil, resume, true, nil
	}
	if err != nil || progress.next == "" {
		// The page did not COMPLETE (a canceled Next context, a mint
		// failure, the teaching expiry, revocation, or an early exhausted
		// entry): its progress is a zero value, and publishing it would
		// wipe the standing cursor with "" — a later Resume() would teach
		// a bare-head restart that skips the entire undelivered backlog.
		// State stays at the last completed page.
		sess.mu.Unlock()
		return records, next, done, err
	}
	sess.replayExhausted = progress.short
	if progress.scannedLast > 0 {
		sess.position = progress.scannedLast
	}
	sess.nextCursor = progress.next
	sess.outstanding -= progress.consumed
	sess.mu.Unlock()
	return records, next, done, err
}

func (sess *listenSession) page(ctx context.Context) ([]ChangeRecord, Cursor, bool, pageProgress, error) {
	sess.mu.Lock()
	dead, exhausted := sess.dead, sess.replayExhausted
	sess.mu.Unlock()
	if dead || exhausted {
		// done=true flows to next(), whose locked publish performs the
		// replay-done transition — never here, mid-page.
		return nil, sess.cursor(), true, pageProgress{}, nil
	}

	// The page read: (position, boundary] under the registration labels —
	// the labels and the boundary are registration-fixed, so a drop or
	// recreate committing mid-replay can neither narrow this range nor mix
	// a successor's records into it. The boundary is ALWAYS bounded, even
	// when it is 0 (the empty log's head): a commit landing before the
	// caller's first Next is the live half's to deliver, and an unbounded
	// read here would replay it too (§6.2's exactly-once).
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", false, pageProgress{}, err
	}
	// The namespace's write pool is a single connection: a transaction that
	// returns without committing or rolling back holds it, and every later
	// write or replay against the namespace blocks indefinitely. The defer
	// makes every path below release it (a no-op after Commit).
	defer tx.Rollback()
	// A replay must never SILENTLY shorten — but it must equally accept what
	// registration found. The loss check is a BASELINE, not span arithmetic:
	// registration counted the rows the log actually retains in (P, R]
	// (holes included — pruning deletes by age, and clock-stepped at stamps
	// leave gaps that a begin registration legitimately replays around),
	// and each page verifies the log still holds exactly that many rows in
	// its outstanding range. Only rows removed AFTER the boundary was fixed
	// count as loss, and that loss is the cursor-expiry teaching error for
	// the caller to reconnect from its last delivered cursor — never a
	// short page reported as done, which would silently omit records the
	// boundary promised.
	var kept int64
	if sess.position < sess.boundary {
		cq, cargs := changeCountSQL(sess.position, sess.boundary, sess.feed)
		if err = tx.QueryRowContext(ctx, cq, cargs...).Scan(&kept); err != nil {
			return nil, "", false, pageProgress{}, err
		}
		if kept != sess.outstanding {
			return nil, "", false, pageProgress{}, fmt.Errorf("listen replay: %w", ErrCursorExpired)
		}
	}
	boundary := sess.boundary
	query, args := changePageSQL(sess.position, &boundary, MaxChangesPageLimit, sess.feed)
	rows, qerr := tx.QueryContext(ctx, query, args...)
	var scanned []loggedChange
	if qerr == nil {
		scanned, qerr = scanChangePage(rows)
		rows.Close()
	}
	// The baseline's bookkeeping half: the rows this page consumed — the
	// count measured at page start minus what remains past the page's last
	// scanned seq, both inside this transaction's snapshot — measured here
	// but applied BELOW, only with the page's other progress after the
	// mint succeeds: a failed mint (the Next context expiring under a slow
	// liveAuthz callback) leaves position and cursor at the pre-page
	// boundary, and a prematurely reduced promise would make the retried
	// page's recount read as loss. The consumed count is the SCAN ITSELF —
	// every scanned feed row is consumed exactly once; the outstanding
	// baseline is fully verified before the scan, so no per-page recount
	// of the tail is needed here.
	consumed := int64(len(scanned))
	if qerr == nil {
		qerr = tx.Commit()
	}
	if qerr != nil {
		return nil, "", false, pageProgress{}, qerr
	}

	// Admit outside the transaction, through the same live authorization
	// gate the live half will deliver through (§6.2: replay pages are
	// filtered exactly like live delivery). A revocation found mid-page
	// OMITS the page: closed must precede everything the caller could still
	// consume (§6.2's exposure rule — a consumer may tear down
	// record-processing state in its closed callback), and the omitted
	// prefix is no loss — the caller reconnects from its last delivered
	// cursor and the durable log re-admits those records on the new session.
	var admitted []loggedChange
	revoked := false
	for _, lc := range scanned {
		vis, rev := sess.admit(lc.rec)
		if rev {
			revoked = true
			break
		}
		if vis {
			admitted = append(admitted, lc)
		}
	}
	if revoked {
		sess.end(ErrListenRevoked)
		return nil, sess.cursor(), true, pageProgress{}, nil
	}

	// The mint runs on FRESH time, not the page's start: the read and the
	// admission callbacks above can span the chain's retention cap, and a
	// mint evaluated against the stale start would keep the already-expired
	// chain and stamp every cursor on it — tokens born dead at return, an
	// immediate resume rejected (chainFor rotates on the actual mint time).
	var mLast int64
	if len(scanned) > 0 {
		mLast = scanned[len(scanned)-1].seq
	}
	records, next, merr := sess.mint(ctx, admitted, sess.position, mLast, len(scanned))
	if merr != nil {
		return nil, "", false, pageProgress{}, merr
	}

	// Exhaustion keys on the SCANNED range, never on the admitted page: a
	// full page whose every record the authorization filtered out is not the
	// boundary — visible records may still sit before it, and ending replay
	// on an empty admitted page would strand them behind a page the caller
	// will never turn.
	short := len(scanned) < MaxChangesPageLimit
	// Page progress stays UNPUBLISHED here: a concurrent Resume() between
	// this point and next()'s dead-check could observe a post-page cursor
	// that the omission then rolls back — a returned cursor cannot be
	// retracted. next() publishes (or drops) the page's progress under one
	// lock, after the omission decision is final.
	progress := pageProgress{short: short, next: next, consumed: consumed}
	if len(scanned) > 0 {
		progress.scannedLast = scanned[len(scanned)-1].seq
	}
	// A short scanned page with nothing to expose IS the boundary — the
	// replay is done, and the live half (6b's next change) takes over from
	// here. A short page with records reports done=false (the final
	// records; the FOLLOWING call reports the boundary, §6.2), and a full
	// page pages on — even one the filter emptied.
	if short && len(records) == 0 {
		return nil, next, true, progress, nil
	}
	return records, next, false, progress, nil
}

// pageProgress is a completed page's unpublished session advance, applied
// by next() only when the page is delivered.
type pageProgress struct {
	short       bool
	scannedLast int64
	next        Cursor
	consumed    int64
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
func (sess *listenSession) end(cause error) {
	sess.mu.Lock()
	if sess.dead {
		sess.mu.Unlock()
		return
	}
	sess.dead = true
	close(sess.stop)    // the poll pump's immediate exit — teardown must not wait out a tick
	sess.ctxCancel()    // and the pumps' in-flight database work — cancel must not wait out a busy timeout
	if cause != nil && sess.pendingClose == nil {
		sess.pendingClose = cause
	}
	sess.cond.Broadcast()
	sess.mu.Unlock()
}

// fireClosed invokes the terminal callback at most once, with the parked
// cause. The callback is caller code running on a pump goroutine, so a panic
// is recovered and logged — deliverCommit's write-path rule, applied here:
// the session is already ending, and a panicking close must not take the
// process down. (recoverPump alone cannot cover this: defers run in reverse,
// and the parked-close flush sits outside the pump's own recover by design.)
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

// mintOne mints a single live record's cursor at its delivery (drain): the
// token rides the chain current at delivery time, never one a rotation
// could have orphaned while the record sat queued. A healthy live-only
// subscription runs no replay pages and no ChangesSince calls, so retention
// pruning rides here every listenPruneInterval deliveries — without it, a
// namespace served only by a long-lived stream would accumulate expired
// token rows and age-eligible records indefinitely despite a nonzero
const listenPruneInterval = 1000

func (sess *listenSession) mintOne(ctx context.Context, seq int64) (Cursor, error) {
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// now after BeginTx, mint's rule: a queued connection wait must not
	// stamp the delivered cursor with an already-expired issuance.
	now := time.Now()
	// The record must still exist, checked ATOMICALLY with the mint: the
	// read path's ro snapshot released before the drainer got here, and a
	// concurrent pruner (another listener's carrier read, another process)
	// may have deleted the scanned, age-eligible row in the gap. Minting a
	// cursor at a deleted position would hand the client a record whose
	// reconnect silently skips the deleted tail — fail loudly instead.
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM _dolmen_changes WHERE seq = ?`, seq).Scan(&exists); err != nil {
		return "", err
	}
	if exists == 0 {
		return "", fmt.Errorf("listen live: %w", ErrCursorExpired)
	}
	tok, err := mintCursorToken(ctx, tx, now, seq, sess.table, sess.chainFor(now, seq))
	if err != nil {
		return "", err
	}
	if sess.delivered.Add(1)%listenPruneInterval == 0 {
		if err := pruneChanges(ctx, tx, now, sess.s.changeRetention); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return tok, nil
}

// flushParkedClose fires a parked terminal from a pump's exit path, once no
// replay page and no delivery is in flight. The park is the exposure rule's
// serialization point (§6.2): closed must never precede a record the session
// already handed out — an in-flight notify has returned before its pump
// reaches this deferred flush, and the other pump's flush waits out the
// delivery flag — and a page minted while the session was ending may still
// be returning, so the parked close waits for it rather than cutting past
// it. The FIRST cause parked wins; later ends are already absorbed by the
// dead flag.
func (sess *listenSession) flushParkedClose() {
	sess.mu.Lock()
	for sess.replayActive || sess.notifyActive {
		sess.cond.Wait()
	}
	cause := sess.pendingClose
	sess.pendingClose = nil
	sess.mu.Unlock()
	if cause != nil {
		sess.fireClosed(cause)
	}
}

// cancel is the returned teardown: it unregisters the listener, ends the
// session without a closed callback, and waits for both pumps — so once it
// returns, no notify or closed call can still be running (the use-after-free
// rule from notify.go's listener contract; the SSE handler returns only
// cancel is the returned teardown: it unregisters the listener, ends the
// session without a closed callback, and waits for the pumps — so once it
// returns, no notify or closed call can still be running (the use-after-free
// rule from notify.go's listener contract; the SSE handler returns only
// after this). The wait is UNCONDITIONAL, which is why the callbacks carry
// the same kind of rule onCommit's registry already documents: cancel must
// not be called from inside notify or closed — the calling goroutine is one
// of the pumps the wait would sit on, and no session-wide flag can tell a
// reentrant call from an external one racing an in-flight callback (only
// the calling goroutine knows, and Go exposes no portable way to ask).
// Call it from outside the session's goroutines, as the SSE handler does.
func (sess *listenSession) cancel() {
	sess.cancelOnce.Do(func() {
		sess.s.untrackSession(sess)
		sess.unregister()
		sess.end(nil)
		sess.pumps.Wait()
	})
}
