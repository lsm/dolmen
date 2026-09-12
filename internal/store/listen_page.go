package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (sess *listenSession) next(ctx context.Context) ([]ChangeRecord, Cursor, bool, error) {
	if err := sess.lockNext(ctx); err != nil {
		return nil, "", false, err
	}
	defer func() { <-sess.flight }()
	sess.mu.Lock()
	resume := sess.nextCursor

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
	defer sess.mu.Unlock()

	if done && err == nil {
		sess.replayDone = true
		sess.cond.Broadcast()
	}
	if sess.dead {

		sess.replayDone = true
		sess.cond.Broadcast()
		return nil, resume, true, sess.endCauseLocked()
	}
	if err != nil || progress.next == "" {
		return records, next, done, err
	}
	sess.replayExhausted = progress.short
	if progress.scannedLast > 0 {
		sess.position = progress.scannedLast
	}
	sess.nextCursor = progress.next
	sess.outstanding -= progress.consumed
	return records, next, done, err
}

func (sess *listenSession) page(ctx context.Context) ([]ChangeRecord, Cursor, bool, pageProgress, error) {
	sess.mu.Lock()
	dead, exhausted := sess.dead, sess.replayExhausted
	sess.mu.Unlock()
	if dead {
		return nil, sess.cursor(), true, pageProgress{}, sess.endCause()
	}
	if exhausted {
		return nil, sess.cursor(), true, pageProgress{}, nil
	}
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		sess.endOnPageFailure(ctx, err)
		return nil, "", false, pageProgress{}, err
	}

	defer tx.Rollback()

	if err := sess.verifyRetained(ctx, tx, sess.position, sess.boundary, sess.outstanding); err != nil {
		sess.endOnPageFailure(ctx, err)
		return nil, "", false, pageProgress{}, err
	}
	boundary := sess.boundary
	query, args := changePageSQL(sess.position, &boundary, MaxChangesPageLimit, sess.feed)
	rows, qerr := tx.QueryContext(ctx, query, args...)
	var scanned []loggedChange
	if qerr == nil {
		scanned, qerr = scanChangePage(rows)
		rows.Close()
	}

	consumed := int64(len(scanned))
	if qerr == nil {
		qerr = tx.Commit()
	}
	if qerr != nil {
		sess.endOnPageFailure(ctx, qerr)
		return nil, "", false, pageProgress{}, qerr
	}

	var mLast int64
	if len(scanned) > 0 {
		mLast = scanned[len(scanned)-1].seq
	}
	var admitted []loggedChange
	for _, lc := range scanned {
		vis, rev := sess.admit(lc.rec)
		if rev {
			sess.end(ErrListenRevoked)
			return nil, sess.cursor(), true, pageProgress{}, sess.endCause()
		}
		if vis {
			admitted = append(admitted, lc)
		}
	}
	records, next, merr := sess.mint(ctx, admitted, sess.position, mLast, len(scanned))
	if merr != nil {
		sess.endOnPageFailure(ctx, merr)
		return nil, "", false, pageProgress{}, merr
	}

	short := len(scanned) < MaxChangesPageLimit

	progress := pageProgress{short: short, next: next, consumed: consumed, scannedLast: mLast}

	if short && len(records) == 0 {
		return nil, next, true, progress, nil
	}
	return records, next, false, progress, nil
}

type pageProgress struct {
	short       bool
	scannedLast int64
	next        Cursor
	consumed    int64
}

func (sess *listenSession) mint(ctx context.Context, admitted []loggedChange, resume int64, scannedLast int64, scannedCount int) ([]ChangeRecord, Cursor, error) {
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()

	now := time.Now()

	if scannedCount > 0 {
		if err := sess.verifyRetained(ctx, tx, resume, scannedLast, int64(scannedCount)); err != nil {
			return nil, "", err
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

func (sess *listenSession) lockNext(ctx context.Context) error {
	if ctx == nil || ctx.Done() == nil {
		sess.flight <- flightToken
		return nil
	}
	select {
	case sess.flight <- flightToken:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var flightToken = struct{}{}

func (sess *listenSession) chainFor(now time.Time, resume int64) *cursorChain {
	if sess.s.changeRetention <= 0 {
		return sess.chain
	}
	rms := int64(sess.s.changeRetention / time.Millisecond)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if now.UnixMilli() < sess.chain.Start+rms {
		return sess.chain
	}
	origin := resume
	if !sess.replayExhausted && sess.position < origin {
		origin = sess.position
	}
	if floor := sess.lowestOwedLocked(); floor > 0 && floor-1 < origin {
		origin = floor - 1
	}
	sess.chain = newCursorChain(now, origin)
	return sess.chain
}

func (sess *listenSession) verifyRetained(ctx context.Context, tx *sql.Tx, from, to, promised int64) error {
	if from >= to || promised == 0 {
		return nil
	}
	cq, cargs := changeCountSQL(from, to, sess.feed)
	var kept int64
	if err := tx.QueryRowContext(ctx, cq, cargs...).Scan(&kept); err != nil {
		return err
	}
	if kept != promised {
		return fmt.Errorf("listen replay: %w", ErrCursorExpired)
	}
	return nil
}
