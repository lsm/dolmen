package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const listenPruneInterval = 1000

func (sess *listenSession) mintOne(ctx context.Context, seq int64) (Cursor, error) {
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	now := time.Now()

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

func (sess *listenSession) drain() {
	defer sess.pumps.Done()
	defer sess.flushParkedClose()
	defer sess.recoverPump("drain")
	for {
		sess.mu.Lock()

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
				cause = sess.pendingDrainClose
				sess.pendingDrainClose = nil
			}
			sess.mu.Unlock()
			sess.end(cause)
			return
		}
		lc := sess.queue[0]
		sess.queue = sess.queue[1:]
		sess.deliveringSeq = lc.seq
		sess.notifyActive = true
		sess.mu.Unlock()

		var fail error
		func() {
			defer func() {
				sess.mu.Lock()
				sess.deliveringSeq = 0
				sess.notifyActive = false
				sess.cond.Broadcast()
				sess.mu.Unlock()
			}()
			tok, merr := sess.mintOne(sess.ctx, lc.seq)
			if merr != nil {
				fail = sess.fillErr(merr)
				return
			}
			lc.rec.Cursor = tok
			sess.notify(lc.rec)
		}()
		if fail != nil {

			sess.endYielding(fail)
			return
		}
	}
}

func (sess *listenSession) recoverPump(half string) {
	if r := recover(); r != nil {
		slog.Error("listen pump panicked; ending session",
			"namespace", sess.nsName, "table", sess.table, "half", half, "panic", r)
		sess.endYielding(fmt.Errorf("listen %s pump panicked: %v", half, r))
	}
}
