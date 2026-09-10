package store

import (
	"context"
	"fmt"
	"time"
)

// The drain half — slice 6b (r7a). Live delivery is record-at-a-time: the
// drain goroutine (next slice) pops the queue, mints the record's cursor
// HERE — at delivery, not at scan — and hands it to notify. This slice
// lands the mint itself; the drain loop, the replay→live handoff that
// gates it, and the parked-close machinery land in the following slices.

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
