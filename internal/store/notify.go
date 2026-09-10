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
// Listen — slice 6b, §6.2/§9.3's replay-then-live session. This change lands
// the REPLAY HALF: registration — the boundary transaction that fixes the
// replay range (P, R], the resume cursor, and the feed's table-lifetime
// labels as one coordinated operation — and the ChangeReplay paging that
// drains it. The LIVE HALF (the registry join that makes replay-then-live
// exactly-once, the fill/drain pumps, notify delivery, and the overflow /
// lifetime teaching closes) is 6b's next change; until it lands, a drained
// replay reports done and notify is never invoked. Capabilities stay FALSE
// for exactly that reason — the engine does not advertise subscribe until
// the whole body exists (§6.2: the capability surface and the implementation
// change together, never contradicting each other).

var (
	// ErrListenRevoked closes a session whose liveAuthz returned ok=false
	// mid-stream (§6.2): the standing read's authorization was revoked or
	// narrowed, and the next event — replay or live — teaching-closes the
	// stream instead of exposing a record the caller may no longer see.
	ErrListenRevoked = errors.New("subscription authorization revoked mid-stream")
)

// listenSession is one Listen registration: the replay state registration
// fixed as ONE coordinated operation — the resume position P, the replay
// boundary R, the page chain, and the feed's table-lifetime labels — and
// every later guarantee hangs off that fixing: the replay covers seq in
// (P, R] under the registered labels, so a drop or a same-name recreate
// committing mid-replay can neither narrow the range nor mix a successor's
// records into it (§9.3). The live half (6b's next change) adds the registry
// join and the pumps that deliver seq > R, which is what makes the
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
	notify    func(ChangeRecord) // invoked only by the live half (6b's next change)
	closedFn  func(cause error)

	// Registration-fixed replay state — immutable once Listen returns,
	// except the chain, which rotates at its retention cap (chainFor).
	position    int64        // resume position P: the client's `from`, resolved
	boundary    int64        // registration boundary R: the replay is (P, R]
	outstanding int64        // loss-check baseline: rows the log retained in (P, R] AT REGISTRATION — holes included; the pages' promise
	chain       *cursorChain // the page chain every token the session mints rides; guarded by mu (chainFor)
	feed        *changeFeed  // nil on the namespace feed; the table feed's labels at registration

	mu              sync.Mutex
	replayExhausted bool
	nextCursor      Cursor // the standing resume cursor, fixed at registration (Listen)
	dead            bool

	closedOnce sync.Once
	cancelOnce sync.Once
}

// Listen implements the engine-declared notification capability (§6.2, §9.3):
// registration fixes the replay boundary and the feed's labels as ONE
// coordinated operation, the replay pages out through ChangeReplay.Next, and
// — once the live half lands — the session pushes live records through
// notify after the replay drains. closed fires at most once, only when the
// ENGINE ends the session, never for a caller-initiated cancel; the returned
// cancel is idempotent.
//
// TODO(6b-live): the live half — the registry join, the fill/drain pumps,
// notify delivery, and the overflow / lifetime teaching closes — is 6b's
// next change; this body serves the replay half only.
// TODO(9d): while auth is off the SSE handler passes a nil liveAuthz (no
// per-event filter); the admission rules below are live the moment a slice
// wires a real re-resolver in.
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
	// than silently narrowing to it (ErrListenLifetimeEnded, live half).
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

	// The boundary: the replay is (P, R], bounded even when R is 0 (the
	// empty log's head — a real bound, never "unbounded"; page below), and
	// everything after R is the live half's to deliver. The write lock this
	// transaction holds (the cursor mint/refresh above) means no writer can
	// interleave between the head reads and the commit — R is the head of a
	// serial-observability point (§0.6).
	if sess.boundary, err = changeHead(ctx, tx); err != nil {
		return nil, nil, err
	}
	// The loss-check baseline, in the same snapshot: the feed's rows the
	// log ACTUALLY retains in (P, R] — not the span arithmetic, because
	// pruning deletes by age and clock-stepped stamps can leave holes a
	// begin registration legitimately replays around (changeBegin selects
	// the first retained record, whatever sits missing beside it), and not
	// the whole namespace's, because a table feed's promise is only its own
	// records — an unrelated table's aged row deleted later must not read
	// as this feed's loss. Only rows removed AFTER this count are the
	// session's to lose.
	if sess.position < sess.boundary {
		cq, cargs := changeCountSQL(sess.position, sess.boundary, sess.feed)
		if err = tx.QueryRowContext(ctx, cq, cargs...).Scan(&sess.outstanding); err != nil {
			return nil, nil, err
		}
	}
	// The standing resume cursor is fixed HERE, at registration, so a session
	// that ends before its first Next — cancelled by its caller, or ended by
	// the engine once the live half lands (an overflow of its own traffic) —
	// still hands back a boundary the caller can resume from EXACTLY where
	// registration stood. Bare and begin starts mint one at the resume
	// position: the undelivered backlog stays behind the cursor, instead of
	// being skipped by a cursorless reconnect that restarts at the head. A
	// presented token was retained above.
	if sess.nextCursor == "" {
		var terr error
		if sess.nextCursor, terr = mintCursorToken(ctx, tx, now, sess.position, table, sess.chain); terr != nil {
			return nil, nil, terr
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return &ChangeReplay{Next: sess.next, Resume: sess.cursor}, sess.cancel, nil
}

// next pages the replay half: records in (position, boundary] in cursor
// order, one bounded page per call, each admitted record carrying a fresh
// cursor on the registration chain. The page carrying the final replay
// records returns done=false; the following call returns done=true with no
// records — the registration boundary (§6.2). A session ended under the
// page — the caller's cancel from another goroutine, or a revocation this
// page's admission found — reports done too, the page OMITTED on
// revocation: the closed callback has already fired, and nothing may be
// exposed after it (§6.2); the omitted records re-deliver on the caller's
// reconnect from its last cursor.
func (sess *listenSession) next(ctx context.Context) ([]ChangeRecord, Cursor, bool, error) {
	sess.mu.Lock()
	resume := sess.nextCursor // the PRE-page boundary: what an omitted page must hand back
	sess.mu.Unlock()
	records, next, done, err := sess.page(ctx)
	sess.mu.Lock()
	omit := sess.dead && len(records) > 0
	if omit {
		// Roll the standing cursor back with the return value: page's
		// dead-guard cannot cover a cancellation landing between its
		// publication and this check, and Resume must never expose a
		// position past records the caller never received.
		sess.nextCursor = resume
	}
	sess.mu.Unlock()
	if omit {
		// The omitted page's own cursor points PAST records the caller never
		// receives — handing it back would teach a resume that skips them.
		// The pre-page boundary is the honest position.
		return nil, resume, true, nil
	}
	return records, next, done, err
}

func (sess *listenSession) page(ctx context.Context) ([]ChangeRecord, Cursor, bool, error) {
	sess.mu.Lock()
	dead, exhausted := sess.dead, sess.replayExhausted
	sess.mu.Unlock()
	if dead || exhausted {
		return nil, sess.cursor(), true, nil
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
		return nil, "", false, err
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
			return nil, "", false, err
		}
		if kept != sess.outstanding {
			return nil, "", false, fmt.Errorf("listen replay: %w", ErrCursorExpired)
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
	// page's recount read as loss.
	var consumed int64
	if qerr == nil && len(scanned) > 0 && sess.position < sess.boundary {
		last := scanned[len(scanned)-1].seq
		var remaining int64
		cq, cargs := changeCountSQL(last, sess.boundary, sess.feed)
		if qerr = tx.QueryRowContext(ctx, cq, cargs...).Scan(&remaining); qerr == nil {
			consumed = kept - remaining
		}
	}
	if qerr == nil {
		qerr = tx.Commit()
	}
	if qerr != nil {
		return nil, "", false, qerr
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
		return nil, sess.cursor(), true, nil
	}

	// The mint runs on FRESH time, not the page's start: the read and the
	// admission callbacks above can span the chain's retention cap, and a
	// mint evaluated against the stale start would keep the already-expired
	// chain and stamp every cursor on it — tokens born dead at return, an
	// immediate resume rejected (chainFor rotates on the actual mint time).
	records, next, merr := sess.mint(ctx, time.Now(), admitted, sess.position)
	if merr != nil {
		return nil, "", false, merr
	}

	// Exhaustion keys on the SCANNED range, never on the admitted page: a
	// full page whose every record the authorization filtered out is not the
	// boundary — visible records may still sit before it, and ending replay
	// on an empty admitted page would strand them behind a page the caller
	// will never turn.
	short := len(scanned) < MaxChangesPageLimit
	sess.mu.Lock()
	// A session that died under this page (the caller's cancel from another
	// goroutine, an engine end) keeps its PRE-page standing cursor: next
	// omits the records, and Resume must not teach a position past records
	// the caller never received. The consumed count applies here too — only
	// with progress that actually committed.
	if !sess.dead {
		sess.replayExhausted = short
		if len(scanned) > 0 {
			sess.position = scanned[len(scanned)-1].seq
		}
		sess.nextCursor = next
		sess.outstanding -= consumed
	}
	sess.mu.Unlock()
	// A short scanned page with nothing to expose IS the boundary — the
	// replay is done, and the live half (6b's next change) takes over from
	// here. A short page with records reports done=false (the final
	// records; the FOLLOWING call reports the boundary, §6.2), and a full
	// page pages on — even one the filter emptied.
	if short && len(records) == 0 {
		return nil, next, true, nil
	}
	return records, next, false, nil
}

// admit is the live per-event authorization gate (§6.2): it runs BEFORE a
// record is exposed, against the record's own target table. visible reports
// whether the record may reach the caller; revoked reports that the
// authorization itself ended the stream — ok=false is a teaching close,
// never a silent skip. The engine stays grant-blind: it filters by the
// returned scope and the record's Owner label, and — on a table feed —
// compares the returned incarnation with the record's Lifetime, so a
// drop-and-recreate between the re-resolver's answer and admission cannot
// carry a stale unscoped decision onto the successor's records.
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
// standing resume cursor.
func (sess *listenSession) mint(ctx context.Context, now time.Time, admitted []loggedChange, resume int64) ([]ChangeRecord, Cursor, error) {
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
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
// very transaction's prune would delete it — so a replay held longer than
// 2R would hand its client dead cursors. A fresh chain roots at the current
// position, exactly the chain a client resubscribing from here starts
// itself: the old backlog the cap exists to bound stays bounded, and every
// delivered cursor stays resolvable. Retention 0 has no caps and never
// rotates.
//
// While the replay is still draining, a rotation's origin floors at the
// replay's outstanding position: a chain rooted at a later position would
// let the same transaction's prune delete age-eligible records the replay
// has not yet paged — silently shortening the gap-free resume the chain
// exists to protect. Once the replay is exhausted the floor lifts; nothing
// below the current position remains to protect.
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
	sess.chain = newCursorChain(now, origin)
	return sess.chain
}

// cursor returns the replay's latest next-page cursor — what a caller that
// stopped paging mid-replay, or a session ended before its first page,
// resumes from (fixed at registration by Listen).
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
// closed fires — once, with it. A nil cause is the caller's cancel: the
// caller already knows, and closed never fires for it (§6.2). Idempotent;
// the first end wins. In this half the only engine-initiated end is the
// replay's own revocation, which runs on the caller's goroutine inside Next
// — closed fires before Next returns done, so nothing can follow it (§6.2's
// exposure rule, held by construction). The live half's ends (overflow,
// lifetime) arrive on the pumps and serialize through it.
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
// itself; the live half's pumps and registry entries arrive with 6b's next
// change, and cancel will wait out the pumps before returning (the
// use-after-free rule from notify.go's listener contract).
func (sess *listenSession) cancel() {
	sess.cancelOnce.Do(func() {
		sess.end(nil)
	})
}
