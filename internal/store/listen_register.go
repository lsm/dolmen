package store

import (
	"context"
	"sync"
	"time"
)

func (s *Store) Listen(ctx context.Context, nsName, table string, from Cursor, nsGen [16]byte, liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool), notify func(ChangeRecord), closed func(cause error)) (*ChangeReplay, func(), error) {
	if notify == nil {
		return nil, nil, invalidf("listen: notify callback is required")
	}

	n, err := s.nsCtx(ctx, nsName)
	if err != nil {
		return nil, nil, err
	}
	sess := &listenSession{
		s: s, n: n, nsName: nsName, table: table, nsGen: nsGen,
		liveAuthz: liveAuthz, notify: notify, closedFn: closed,
		flight: make(chan struct{}, 1),
	}

	sess.cond = sync.NewCond(&sess.mu)
	sess.stop = make(chan struct{})

	sess.ctx, sess.ctxCancel = context.WithCancel(ctx)

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

	now := time.Now()

	if table != "" {
		var ferr error
		if sess.feed, ferr = changeFeedOf(ctx, tx, nsName, table); ferr != nil {
			return nil, nil, ferr
		}
	}

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

		sess.nextCursor = from
	}

	if sess.boundary, err = changeHead(ctx, tx); err != nil {
		return nil, nil, err
	}

	if liveAuthz != nil && sess.position < sess.boundary {
		if scope, _, ok := liveAuthz(table); ok && scope != nil {
			stale, serr := unlabeledChangeInRange(ctx, tx, sess.position, sess.boundary, sess.feed)
			if serr != nil {
				return nil, nil, serr
			}
			if stale {
				return nil, nil, ErrScopedFeedPredatesLabels
			}
		}
	}

	if sess.position < sess.boundary {
		cq, cargs := changeCountSQL(sess.position, sess.boundary, sess.feed)
		if err = tx.QueryRowContext(ctx, cq, cargs...).Scan(&sess.outstanding); err != nil {
			return nil, nil, err
		}
	}

	if sess.nextCursor == "" {
		var terr error
		if sess.nextCursor, terr = mintCursorToken(ctx, tx, now, sess.position, table, sess.chain); terr != nil {
			return nil, nil, terr
		}
	}

	sess.liveRead = sess.boundary
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	committed = true

	sess.mu.Lock()
	sess.pumpsLaunched = true
	sess.mu.Unlock()
	sess.pumps.Add(3)
	go sess.pump()
	go sess.drain()
	go sess.pollWake()
	return &ChangeReplay{Next: sess.next, Resume: sess.cursor}, sess.cancel, nil
}
