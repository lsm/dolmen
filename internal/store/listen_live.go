package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const ListenQueueBound = 8 * MaxChangesPageLimit

var ErrListenOverflow = errors.New("subscription buffer overflow: the subscriber drained slower than commits arrived")

var ErrListenLifetimeEnded = errors.New("subscription target's lifetime ended")

var ErrListenRevoked = errors.New("subscription authorization revoked")

var ErrListenAged = errors.New("subscription reached the configured age bound")

func (sess *listenSession) admit(rec ChangeRecord) (visible bool, cause error) {
	if sess.liveAuthz == nil {
		return true, nil
	}
	scope, inc, ok := sess.liveAuthz(rec.Table)
	if !ok {
		return false, ErrListenRevoked
	}
	if scope != nil {
		if scope.Empty {
			return false, nil
		}
		if rec.Owner == "" {
			return false, ErrScopedFeedPredatesLabels
		}
		if scope.Owner != rec.Owner {
			return false, nil
		}
	}
	if inc != (Incarnation{}) {
		if inc.NsGen != rec.Lifetime.NsGen {
			return false, nil
		}
		if sess.table != "" {
			if inc.Table != rec.Lifetime.Table || inc.DropGen != rec.Lifetime.DropGen {
				return false, nil
			}
		}
	}
	return true, nil
}

func (sess *listenSession) wake(table string, changes ChangeRange) {
	sess.mu.Lock()
	sess.woken = true
	sess.cond.Broadcast()
	sess.mu.Unlock()
}

func (sess *listenSession) pump() {
	defer sess.pumps.Done()
	defer sess.flushParkedClose()
	defer sess.recoverPump("fill")
	if cause := sess.fill(); cause != nil {

		sess.endYielding(cause)
	}
}

const listenPollInterval = 250 * time.Millisecond

func (sess *listenSession) pollInterval() time.Duration {
	if sess.s == nil || sess.s.changeRetention <= 0 || sess.s.changeRetention/2 >= listenPollInterval {
		return listenPollInterval
	}
	if d := sess.s.changeRetention / 2; d < time.Millisecond {
		return time.Millisecond
	} else {
		return d
	}
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
			if c := sess.explicitCause(); c != nil {
				sess.end(c)
				return
			}
			sess.wake("", ChangeRange{})
			sess.protectQueue()
		}
	}
}

func (sess *listenSession) explicitCause() error {
	c := context.Cause(sess.ctx)
	if c == nil || errors.Is(c, context.DeadlineExceeded) || errors.Is(c, context.Canceled) {
		return nil
	}
	return c
}

func (sess *listenSession) fill() error {
	for {
		sess.mu.Lock()
		for !sess.dead && !sess.woken {
			sess.cond.Wait()
		}
		closing := sess.dead || sess.pendingClose != nil || sess.pendingDrainClose != nil
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
				break
			}
		}
	}
}

func (sess *listenSession) isClosing() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.dead || sess.pendingClose != nil || sess.pendingDrainClose != nil
}

func (sess *listenSession) fillBatch() (read int, err error) {
	ctx := sess.ctx
	sess.protectQueue()
	scanned, ended, rerr := sess.readBatch(ctx)
	if rerr != nil {
		return 0, sess.fillErr(rerr)
	}
	var admitted []loggedChange
	revoked := false
	var revokeCause error
	for _, lc := range scanned {
		vis, cause := sess.admit(lc.rec)
		if cause != nil {
			revoked = true
			revokeCause = cause
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
			sess.pendingDrainClose = revokeCause
		}
		sess.cond.Broadcast()
		sess.mu.Unlock()
		if wasEmpty && len(admitted) > 0 {
			sess.protectQueue()
		}
		return 0, nil
	}
	sess.mu.Lock()
	if len(scanned) > 0 {
		sess.liveRead = scanned[len(scanned)-1].seq
	}
	wasEmpty := len(sess.queue) == 0
	sess.queue = append(sess.queue, admitted...)
	if len(admitted) > 0 {
		sess.cond.Broadcast()
	}

	over := len(sess.queue) > sess.bound()
	short := len(scanned) < MaxChangesPageLimit

	for over && ended && !sess.dead {
		sess.cond.Wait()
		over = len(sess.queue) > sess.bound()
	}
	dead := sess.dead
	if ended && short {

		sess.pendingDrainClose = ErrListenLifetimeEnded
	}
	sess.cond.Broadcast()
	sess.mu.Unlock()
	if wasEmpty && len(admitted) > 0 && !dead {
		sess.protectQueue()
	}
	if dead {
		return 0, nil
	}
	if ended && short {
		return 0, nil
	}
	if over && !ended {

		return 0, ErrListenOverflow
	}
	return len(scanned), nil
}

func (sess *listenSession) readBatch(ctx context.Context) (scanned []loggedChange, lifetimeEnded bool, err error) {
	prune := sess.s != nil && sess.s.pruneDue(sess.nsName, time.Now())
	var tx *sql.Tx
	if prune {
		tx, err = sess.n.rw.BeginTx(ctx, nil)
	} else {
		tx, err = sess.n.ro.BeginTx(ctx, nil)
	}
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	sess.mu.Lock()
	from := sess.liveRead
	sess.mu.Unlock()
	query, args := changePageSQL(from, nil, MaxChangesPageLimit, sess.feed, nil)
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
			lifetimeEnded = true
		case ferr != nil:
			return nil, false, ferr
		case current.nsgen != sess.feed.nsgen || current.dropGen != sess.feed.dropGen:
			lifetimeEnded = true
		}
	}
	if prune {
		if len(scanned) > 0 {
			if _, err := mintCursorToken(ctx, tx, time.Now(), from, sess.table, sess.chainFor(time.Now(), from)); err != nil {
				return nil, false, err
			}
		}
		if err := pruneChanges(ctx, tx, time.Now(), sess.s.changeRetention); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return scanned, lifetimeEnded, nil
}

func (sess *listenSession) fillErr(err error) error {
	if c := sess.explicitCause(); c != nil {
		return c
	}
	if errors.Is(err, ErrNotFound) {
		return ErrListenLifetimeEnded
	}
	if sess.s != nil && sess.s.nsEvicted(sess.nsName, sess.n) {
		return ErrListenLifetimeEnded
	}
	return fmt.Errorf("listen fill %s: %w", sess.nsName, err)
}

func (sess *listenSession) lowestOwedLocked() int64 {
	floor := sess.deliveringSeq
	if len(sess.queue) > 0 && (floor == 0 || sess.queue[0].seq < floor) {
		floor = sess.queue[0].seq
	}
	return floor
}

func (sess *listenSession) protectQueue() {
	if sess.s == nil {
		return
	}
	rms := int64(sess.s.changeRetention / time.Millisecond)
	if rms <= 0 {
		return
	}
	sess.mu.Lock()
	floor, chain := sess.lowestOwedLocked(), sess.queueChain
	sess.mu.Unlock()
	if floor == 0 {
		return
	}
	if chain != nil {
		margin := rms / 2
		if tick := 2 * int64(sess.pollInterval()/time.Millisecond); tick > margin {
			margin = tick
		}
		if time.Now().UnixMilli() < chain.Start+rms-margin {
			return
		}
	}
	ctx := sess.ctx
	failed := func(stage string, err error) {
		if ctx.Err() == nil {
			slog.Error("listen queue protection: "+stage, "namespace", sess.nsName, "err", err)
		}
	}
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		failed("begin", err)
		return
	}
	defer tx.Rollback()
	now := time.Now()
	replacement := newCursorChain(now, floor-1)
	if _, err := mintCursorToken(ctx, tx, now, floor, sess.table, replacement); err != nil {
		failed("mint", err)
		return
	}
	if err := pruneChanges(ctx, tx, now, sess.s.changeRetention); err != nil {
		failed("prune", err)
		return
	}
	if err := tx.Commit(); err != nil {
		failed("commit", err)
		return
	}
	sess.mu.Lock()
	sess.queueChain = replacement
	sess.mu.Unlock()
}

func (sess *listenSession) bound() int {
	if sess.queueBound > 0 {
		return sess.queueBound
	}
	return ListenQueueBound
}
