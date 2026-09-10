package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// The drain half — slice 6b (r7a: the delivery mint; this slice: the
// loop). Live delivery is record-at-a-time: the drain goroutine pops the
// queue, mints the record's cursor HERE — at delivery, not at scan — and
// hands it to notify. The parked-close machinery (a terminal's
// serialization against in-flight work) lands in the following slice.

// listenPruneInterval: periodic retention pruning rides the delivery mint
// every N deliveries — without it, a namespace served only by a long-lived
// stream would accumulate expired token rows and age-eligible records
// indefinitely despite a nonzero retention (replay pages and the write
// paths prune, but a purely-live session never turns a replay page).
const listenPruneInterval = 1000

// mintOne mints the cursor for ONE delivered live record, in its own
// transaction, stamped at delivery time so every delivered cursor rides
// the chain current NOW — a queued record never holds a cursor a rotation
// could orphan while it waited behind a slow subscriber (§9.3's chain
// cap), and every delivered cursor is as fresh as its delivery.
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
	// fill's read snapshot released before the delivery got here, and a
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

// drain is the delivery half: pop the queue one record at a time, mint
// the record's cursor at its delivery, hand it to notify. Delivery is
// gated on the replay having drained — the boundary call completed, no
// page still in flight — so the concatenation §6.2 promises arrives as
// replay-then-live in order: nothing live reaches the caller before its
// replay has fully reported. notifyActive brackets each delivery's whole
// mint-and-notify span, so the parked-close machinery (the next slice)
// can wait out an in-flight delivery instead of firing a terminal
// between a mint and its notify. A mint failure is a teaching end — the
// record the client is about to be handed must still resolve — and the
// session ends with the wrapped cause, on this counted goroutine (r6d's
// rule: the fire rides the pump through teardown).
func (sess *listenSession) drain() {
	defer sess.pumps.Done()
	defer sess.recoverPump("drain")
	for {
		sess.mu.Lock()
		// Live delivery is gated on the boundary call having COMPLETED —
		// not merely on the boundary being reached — and on no page still
		// being in flight: replayDone flips inside next()'s locked
		// publish, so the caller has its final result by the time this
		// goroutine can proceed, and replayActive holds off delivery
		// while any later page (the caller keeps paging past done) is
		// being decided.
		for !sess.dead && (!sess.replayDone || sess.replayActive || len(sess.queue) == 0) {
			sess.cond.Wait()
		}
		if sess.dead {
			sess.mu.Unlock()
			return
		}
		lc := sess.queue[0]
		sess.queue = sess.queue[1:]
		sess.notifyActive = true
		sess.mu.Unlock()

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
// goroutines. The session ends with the recovered panic as its cause:
// the stream is already unreliable from the caller's perspective, and
// the durable log recovers everything queued behind it.
func (sess *listenSession) recoverPump(half string) {
	if r := recover(); r != nil {
		slog.Error("listen pump panicked; ending session",
			"namespace", sess.nsName, "table", sess.table, "half", half, "panic", r)
		sess.end(fmt.Errorf("listen %s pump panicked: %v", half, r))
	}
}
