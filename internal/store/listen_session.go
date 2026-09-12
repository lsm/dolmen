package store

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
)

var errListenEnded = errors.New("listen: the subscription ended")

type listenSession struct {
	s      *Store
	n      *nsDB
	nsName string
	table  string
	nsGen  [16]byte

	liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool)
	notify    func(ChangeRecord)
	closedFn  func(cause error)

	position    int64
	boundary    int64
	outstanding int64
	chain       *cursorChain
	queueChain  *cursorChain
	feed        *changeFeed

	mu         sync.Mutex
	cond       *sync.Cond
	flight     chan struct{}
	woken      bool
	pumps      sync.WaitGroup
	unregister func()

	stop chan struct{}

	queue    []loggedChange
	liveRead int64

	pendingClose      error
	pendingCloseYield bool
	pendingDrainClose error

	pumpsLaunched bool

	firing bool

	ctx       context.Context
	ctxCancel context.CancelFunc

	delivered     atomic.Int64
	deliveringSeq int64

	nextCursor      Cursor
	replayExhausted bool
	dead            bool

	replayDone   bool
	replayActive bool
	notifyActive bool

	closedOnce sync.Once
	cancelOnce sync.Once
}

func (sess *listenSession) cursor() Cursor {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.nextCursor
}

func (sess *listenSession) end(cause error) {
	first := sess.endParked(cause)
	if cause == nil || !first {
		return
	}
	sess.mu.Lock()
	if sess.pumpsLaunched || sess.pendingClose == nil {
		sess.mu.Unlock()
		return
	}

	cause = sess.pendingClose
	sess.pendingClose = nil
	for sess.replayActive || sess.notifyActive {
		sess.cond.Wait()
	}
	sess.mu.Unlock()
	sess.fireClosed(cause)
}

func (sess *listenSession) endParked(cause error) bool {
	return sess.endPark(cause, false)
}

func (sess *listenSession) endOnPageFailure(ctx context.Context, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	sess.endYielding(err)
}

func (sess *listenSession) endYielding(cause error) bool {
	return sess.endPark(cause, true)
}

func (sess *listenSession) endCause() error {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.endCauseLocked()
}

func (sess *listenSession) endCauseLocked() error {
	if sess.pendingClose != nil {
		return sess.pendingClose
	}
	return errListenEnded
}

func (sess *listenSession) endPark(cause error, yields bool) bool {
	sess.mu.Lock()
	first := !sess.dead
	if first {
		sess.dead = true
		close(sess.stop)
	}

	if cause != nil {
		if first && sess.pendingClose == nil {
			sess.pendingClose = cause
			sess.pendingCloseYield = yields
		} else if !first && !yields && sess.pendingClose != nil && sess.pendingCloseYield {
			sess.pendingClose = cause
			sess.pendingCloseYield = false
		}
	}
	if first {
		sess.ctxCancel()
		sess.cond.Broadcast()
		sess.mu.Unlock()
		return true
	}
	sess.mu.Unlock()
	return false
}

func (sess *listenSession) flushParkedClose() {
	sess.mu.Lock()
	for sess.pendingClose != nil && (sess.replayActive || sess.notifyActive) {
		sess.cond.Wait()
	}
	if sess.pendingClose != nil && sess.pendingCloseYield && sess.s != nil {

		sess.mu.Unlock()
		sess.s.mu.Lock()
		sess.s.mu.Unlock()
		sess.mu.Lock()
	}
	cause := sess.pendingClose
	sess.pendingClose = nil
	sess.mu.Unlock()
	if cause != nil {
		sess.fireClosed(cause)
	}
}

func (sess *listenSession) fireClosed(cause error) {
	sess.closedOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("listen closed callback panicked; session already ending",
					"namespace", sess.nsName, "table", sess.table, "panic", r)
			}
		}()
		if sess.closedFn != nil {
			sess.mu.Lock()
			sess.firing = true
			sess.mu.Unlock()
			defer func() {
				sess.mu.Lock()
				sess.firing = false
				sess.mu.Unlock()
			}()
			sess.closedFn(cause)
		}
	})
}

func (sess *listenSession) cancel() {
	sess.cancelOnce.Do(func() {
		if sess.s != nil {
			sess.s.untrackSession(sess)
		}
		if sess.unregister != nil {
			sess.unregister()
		}
		sess.end(nil)
	})
	sess.mu.Lock()
	firing := sess.firing
	sess.mu.Unlock()
	if !firing {
		sess.pumps.Wait()
	}
}
