package store

import (
	"context"
	"errors"
	"fmt"
)

// The live half — slices 6b (r6a, r6b). Listen joins the commit registry
// BEFORE its boundary transaction (the register-and-replay ordering: a
// record can only take seq > R by committing after that transaction —
// SQLite serializes writers through the same rw connection, so its commit
// necessarily observes the registration — making replay (P, R] and live
// > R disjoint by construction, §6.2's exactly-once). The fill pump
// pages the durable log on every wake into the interim queue, and the
// bound caps it: a subscriber whose own traffic outruns its drain meets
// the overflow teaching close. The drain (delivery) and the handoff
// gating land in the following slices.

// listenQueueBound caps the records buffered for one subscriber: the
// interim commits landing while the replay drains, plus live records
// queued behind a slow client (§6.2). It is deliberately a multiple of
// the page bound — a single bulk commit the size of a page must not
// overflow a fresh subscriber — and only records the page actually
// scanned ever occupy it, so it can be tripped by nothing but the
// subscriber's own traffic.
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

// fill is the live read half: on every wake it pages the durable log
// forward from liveRead and queues what it finds. It shares the rw pool
// with Next and the write paths, so its transactions serialize with them;
// it never holds sess.mu across database work. A queue past
// listenQueueBound ends the session with the teaching close.
func (sess *listenSession) fill() {
	defer sess.pumps.Done()
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
			read, ferr := sess.fillBatch()
			if ferr != nil {
				sess.end(ferr)
				return
			}
			if read < MaxChangesPageLimit {
				break // drained to the head; wait for the next wake
			}
		}
	}
}

// fillBatch reads one bounded batch of live records and queues them.
// liveRead advances past every record read, visible or not: an invisible
// record is delivered never, but its position is consumed exactly once.
func (sess *listenSession) fillBatch() (read int, err error) {
	ctx := sess.ctx // the session's scope: cancel aborts in-flight database work, not just future reads
	scanned, rerr := sess.readBatch(ctx)
	if rerr != nil {
		return 0, rerr
	}
	sess.mu.Lock()
	if len(scanned) > 0 {
		sess.liveRead = scanned[len(scanned)-1].seq
	}
	sess.queue = append(sess.queue, scanned...)
	over := len(sess.queue) > listenQueueBound
	sess.cond.Broadcast()
	sess.mu.Unlock()
	if over {
		// The teaching reconnect: the stream ends and the client resumes
		// from its last delivered cursor — the durable log is the catch-up
		// path; the buffer never was the durability mechanism.
		sess.end(ErrListenOverflow)
		return 0, nil
	}
	return len(scanned), nil
}

// readBatch is the live half's read transaction: seq in (liveRead, head]
// in order, one bounded page, with a table feed's registration-label
// filter and a current-lifetime check — a target that moved under the
// session ends it (a drop, or a drop-and-recreate: a same-named successor
// is a different feed) instead of silently narrowing.
func (sess *listenSession) readBatch(ctx context.Context) ([]loggedChange, error) {
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, sess.fillErr(err)
	}
	defer tx.Rollback()
	if sess.feed != nil {
		current, ferr := changeFeedOf(ctx, tx, sess.nsName, sess.feed.table)
		if ferr != nil {
			return nil, sess.fillErr(ferr)
		}
		if current.nsgen != sess.feed.nsgen || current.dropGen != sess.feed.dropGen {
			return nil, ErrListenLifetimeEnded
		}
	}
	sess.mu.Lock()
	from := sess.liveRead
	sess.mu.Unlock()
	query, args := changePageSQL(from, nil, MaxChangesPageLimit, sess.feed)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, sess.fillErr(err)
	}
	scanned, err := scanChangePage(rows)
	rows.Close()
	if err != nil {
		return nil, sess.fillErr(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, sess.fillErr(err)
	}
	return scanned, nil
}

// fillErr maps a fill failure onto the session's close causes: a
// namespace the read can no longer find is the teaching lifetime end;
// everything else is an engine failure the caller sees verbatim. (The
// evicted-pool variant — dropped and evicted, or replaced by a recreated
// successor — lands with the pool-paths slice's nsEvicted helper.)
func (sess *listenSession) fillErr(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrListenLifetimeEnded
	}
	return fmt.Errorf("listen fill %s: %w", sess.nsName, err)
}
