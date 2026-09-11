package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// The live half — slices 6b (r6a–r6d). Listen joins the commit registry
// BEFORE its boundary transaction (the register-and-replay ordering: a
// record can only take seq > R by committing after that transaction —
// SQLite serializes writers through the same rw connection, so its commit
// necessarily observes the registration — making replay (P, R] and live
// > R disjoint by construction, §6.2's exactly-once). The fill pump
// pages the durable log on every wake into the interim queue, and the
// bound caps it: a subscriber whose own traffic outruns its drain meets
// the overflow teaching close. The drain that delivers the queue — gated
// on the replay's boundary call — is listen_drain.go's.

// listenQueueBound caps the records buffered for one subscriber: the
// interim commits landing while the replay drains, plus live records
// queued behind a slow client (§6.2). It is deliberately a multiple of
// the page bound — a single bulk commit the size of a page must not
// overflow a fresh subscriber. What the page scanned occupies the queue,
// visible or not: exact for table feeds (the SQL feed filter) and, while
// auth is off, for namespace feeds too (liveAuthz is nil, nothing is
// invisible) — so the bound is tripped by nothing but the subscriber's
// own traffic. Once scoping lands, keeping foreign records OUT of the
// queue is the admission gate's §6.2 promise (engine.go's Listen
// contract), never the bound's — see TODO(9d) on the loss baseline for
// the analogous pre-filter count.
const listenQueueBound = 8 * MaxChangesPageLimit

// ErrListenOverflow is the teaching close for a subscriber whose own
// visible traffic outran its drain: the session's bounded buffer filled
// (§6.2, §9.3). The stream ends and the client resumes from its last
// persisted cursor — the durable log is the durability mechanism; the
// buffer never is.
var ErrListenOverflow = errors.New("subscription buffer overflow: the subscriber drained slower than commits arrived")

// ErrListenLifetimeEnded closes a session whose feed target ended under
// it: the subscribed table was dropped (a table feed follows ONE table
// lifetime, §9.3 — a successor of the same name is a different feed), or
// the namespace was dropped or replaced. The session is bound to the
// namespace instance it registered on — a recreated namespace restarts
// the change sequence, and a predecessor's session must never follow into
// the successor (§9.3's cursor-lifetime rule, applied to streams).
var ErrListenLifetimeEnded = errors.New("subscription target's lifetime ended")

var ErrListenRevoked = errors.New("subscription authorization revoked")

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
	if inc != (Incarnation{}) {
		if inc.NsGen != rec.Lifetime.NsGen {
			return false, false
		}
		if sess.table != "" {
			if inc.Table != rec.Lifetime.Table || inc.DropGen != rec.Lifetime.DropGen {
				return false, false
			}
		}
	}
	return true, false
}

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

// pump is the registered goroutine: fill until a terminal, then end the
// session with it. The count spans the WHOLE goroutine — the terminal
// teardown included — so a cancel that joins the session waits out the
// parked close the deferred flush fires: a caller freeing what closedFn
// captures the moment cancel returns is the use-after-free rule from
// notify.go's listener contract. The one exception a counted pump cannot
// honor is a closedFn that itself calls cancel — no goroutine can wait
// itself out — and that is what the firing flag carves out (fireClosed).
func (sess *listenSession) pump() {
	defer sess.pumps.Done()
	defer sess.flushParkedClose()
	defer sess.recoverPump("fill")
	if cause := sess.fill(); cause != nil {
		sess.end(cause)
	}
}

const listenPollInterval = 250 * time.Millisecond

func (sess *listenSession) pollInterval() time.Duration {
	if sess.s != nil && sess.s.changeRetention > 0 && sess.s.changeRetention/2 < listenPollInterval {
		return sess.s.changeRetention / 2
	}
	return listenPollInterval
}

func (sess *listenSession) pollWake() {
	defer sess.pumps.Done()
	defer sess.recoverPump("poll")
	t := time.NewTicker(sess.pollInterval())
	defer t.Stop()
	for {
		select {
		case <-sess.stop:
			return
		case <-t.C:
			sess.wake("", ChangeRange{})
			sess.protectQueue(false)
		}
	}
}

// fill is the live read half: on every wake it pages the durable log
// forward from liveRead and queues what it finds. It shares the rw pool
// with Next and the write paths, so its transactions serialize with them;
// it never holds sess.mu across database work. A queue past
// listenQueueBound is one terminal it reports for pump to end the session
// with — the teaching close — and a target whose lifetime ended under the
// session ends it through the queue-owned close (readBatch), after the
// predecessor's committed records have paged through.
func (sess *listenSession) fill() error {
	for {
		sess.mu.Lock()
		for !sess.dead && !sess.woken {
			sess.cond.Wait()
		}
		closing := sess.dead || sess.pendingClose != nil || sess.pendingDrainClose != nil // isClosing's check, inline under the held lock
		sess.woken = false
		sess.mu.Unlock()
		if closing {
			return nil
		}

		for {
			if sess.isClosing() {
				return nil
			}
			read, ferr := sess.fillBatch()
			if ferr != nil {
				return ferr
			}
			if read < MaxChangesPageLimit {
				break // drained to the head; wait for the next wake
			}
		}
	}
}

// isClosing reports whether the session must take no further reads: it is
// dead, or a terminal is parked. A parked queue-owned terminal means the
// feed's world already ended under the session (its armer says which
// way) — a later fill must read nothing further; the drainer delivers the
// admitted prefix and fires the parked close at the empty queue.
func (sess *listenSession) isClosing() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.dead || sess.pendingClose != nil || sess.pendingDrainClose != nil
}

// fillBatch reads one bounded batch of live records and queues them.
// liveRead advances past every record read, visible or not: an invisible
// record is delivered never, but its position is consumed exactly once.
func (sess *listenSession) fillBatch() (read int, err error) {
	ctx := sess.ctx // the session's scope: cancel aborts in-flight database work, not just future reads
	sess.protectQueue(false)
	scanned, ended, rerr := sess.readBatch(ctx)
	if rerr != nil {
		return 0, sess.fillErr(rerr)
	}
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
		sess.mu.Lock()
		if len(scanned) > 0 {
			sess.liveRead = scanned[len(scanned)-1].seq
		}
		if sess.dead {
			sess.mu.Unlock()
			return 0, nil
		}
		wasEmpty := len(sess.queue) == 0
		sess.queue = append(sess.queue, admitted...)
		if sess.pendingDrainClose == nil {
			sess.pendingDrainClose = ErrListenRevoked
		}
		sess.cond.Broadcast()
		sess.mu.Unlock()
		if wasEmpty && len(admitted) > 0 {
			sess.protectQueue(true)
		}
		return 0, nil
	}
	sess.mu.Lock()
	if len(scanned) > 0 {
		sess.liveRead = scanned[len(scanned)-1].seq
	}
	wasEmpty := len(sess.queue) == 0
	sess.queue = append(sess.queue, admitted...)
	// The bound is measured INSIDE the critical section: the broadcast
	// below can wake the drainer, which pops the queue under this same
	// lock — an unlocked len() races the slice header and may miss an
	// over-limit moment the drainer already shrank past.
	over := len(sess.queue) > listenQueueBound
	short := len(scanned) < MaxChangesPageLimit
	// An ENDED feed drains its predecessor backlog under BACKPRESSURE,
	// not around the bound: the backlog is not size-bounded (retention
	// may be disabled, and time-based retention caps age, never volume),
	// so queueing it whole could exhaust process memory — and the
	// overflow teaching close is the wrong terminal against a dropped
	// target, whose reconnect remedy cannot succeed. The fill parks until
	// the drainer frees capacity below the bound, then pages on: the
	// predecessor still delivers in full before the close arms at the
	// short tail — only its arrival in memory is chunked, never past
	// bound + one page (codex P1 on #230, thread r3984838673). The
	// queue's durable protection does NOT ride this park — the poll
	// pump's standing refresh owns it (pollWake), which is what keeps a
	// queue parked past the fill's own exit protected too.
	for over && ended && !sess.dead {
		sess.cond.Wait()
		over = len(sess.queue) > listenQueueBound
	}
	dead := sess.dead // read under the lock the wait (or the fall-through) still holds
	if ended && short {
		// The feed's target ended under the session — and the scan, run
		// FIRST under the registration labels, has reached the feed's
		// SHORT tail: every predecessor record this session owes has
		// queued. The close is queue-owned: the drainer delivers the
		// final predecessor batch, then fires ErrListenLifetimeEnded at
		// the empty queue. A FULL page with ended does NOT arm — more
		// predecessor records remain behind it, and the fill keeps
		// paging them (read = the page bound) until the short tail arms
		// the close; arming early would strand every later batch behind
		// isClosing (codex P1 on #230, thread r3984677145; Δ3 itself was
		// codex P1 on #220).
		sess.pendingDrainClose = ErrListenLifetimeEnded
	}
	sess.cond.Broadcast()
	sess.mu.Unlock()
	if wasEmpty && len(admitted) > 0 && !dead {
		sess.protectQueue(true)
	}
	if dead {
		return 0, nil // the backpressure outlived the session: nothing more is ours to read
	}
	if ended && short {
		return 0, nil // read 0: the fill's batch loop ends; the drain owns the close
	}
	if over && !ended {
		// The teaching reconnect: the stream ends and the client resumes
		// from its last delivered cursor — the durable log is the catch-up
		// path; the buffer never was the durability mechanism. Reported,
		// not fired: pump ends the session with it at one fire point,
		// outside this loop. An ended feed never takes it — the backpressure
		// above holds the bound, and the lifetime close at the short tail
		// is the honest terminal against a dropped target (the
		// overflow-trip variant closed a 10k backlog with one record
		// delivered, behind a reconnect that cannot succeed — fresh-context
		// adversarial pass on #230).
		return 0, ErrListenOverflow
	}
	return len(scanned), nil
}

// readBatch is the live half's read transaction: seq in (liveRead, head]
// in order, one bounded page, under the table feed's registration-label
// filter. The current-lifetime check runs AFTER the scan, in the same
// transaction: the labels a session reads under are registration-fixed,
// so a predecessor lifetime's records still match them after a drop (or
// a drop-and-recreate: a same-named successor is a different feed)
// commits — scanning first is what delivers those records instead of
// losing them when the drop takes the write connection ahead of the
// fill (Δ3, codex P1 on #220: the check-first ordering closed the
// session with events after liveRead never queued, and a reconnect on
// the old feed cannot recover them).
func (sess *listenSession) readBatch(ctx context.Context) (scanned []loggedChange, lifetimeEnded bool, err error) {
	tx, err := sess.n.ro.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	sess.mu.Lock()
	from := sess.liveRead
	sess.mu.Unlock()
	query, args := changePageSQL(from, nil, MaxChangesPageLimit, sess.feed)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	scanned, err = scanChangePage(rows)
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	if sess.feed != nil {
		current, ferr := changeFeedOf(ctx, tx, sess.nsName, sess.feed.table)
		switch {
		case ferr != nil && errors.Is(ferr, ErrNotFound):
			lifetimeEnded = true // the table is gone: a drop ended this feed
		case ferr != nil:
			return nil, false, ferr
		case current.nsgen != sess.feed.nsgen || current.dropGen != sess.feed.dropGen:
			lifetimeEnded = true // a same-named successor is a different feed
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return scanned, lifetimeEnded, nil
}

// fillErr maps a fill failure onto the session's close causes: a
// namespace the read can no longer find — gone from the registry, or
// served by a different instance than the session registered on (dropped
// and evicted, replaced by a recreated successor) — is the teaching
// lifetime end; everything else is an engine failure the caller sees
// verbatim. The eviction check takes s.mu, which a DropNamespace's evict
// holds while waiting out this namespace's open connections — so callers
// must hold NO transaction of the namespace's pools when they map through
// here (readBatch returns raw errors for exactly that reason; codex P1 on
// #236, thread r3985954997).
func (sess *listenSession) fillErr(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrListenLifetimeEnded
	}
	if sess.s != nil && sess.s.nsEvicted(sess.nsName, sess.n) {
		return ErrListenLifetimeEnded
	}
	return fmt.Errorf("listen fill %s: %w", sess.nsName, err)
}

func (sess *listenSession) protectQueue(force bool) {
	if sess.s == nil || sess.s.changeRetention <= 0 {
		return
	}
	sess.mu.Lock()
	var head int64
	if len(sess.queue) > 0 {
		head = sess.queue[0].seq
	}
	var start int64
	if sess.chain != nil {
		start = sess.chain.Start
	}
	sess.mu.Unlock()
	if head == 0 {
		return
	}
	rms := int64(sess.s.changeRetention / time.Millisecond)
	if !force && time.Now().UnixMilli() < start+rms-int64(listenPollInterval/time.Millisecond) {
		return
	}
	ctx := sess.ctx
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("listen queue protection: begin", "namespace", sess.nsName, "err", err)
		return
	}
	defer tx.Rollback()
	now := time.Now()
	sess.mu.Lock()
	origin := sess.liveRead
	if !sess.replayExhausted && sess.position < origin {
		origin = sess.position
	}
	if head-1 < origin {
		origin = head - 1
	}
	replacement := newCursorChain(now, origin)
	sess.mu.Unlock()
	if _, err := mintCursorToken(ctx, tx, now, head, sess.table, replacement); err != nil {
		slog.Error("listen queue protection: mint", "namespace", sess.nsName, "err", err)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Error("listen queue protection: commit", "namespace", sess.nsName, "err", err)
		return
	}
	sess.mu.Lock()
	sess.chain = replacement
	sess.mu.Unlock()
}
