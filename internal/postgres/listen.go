package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/store"
)

const listenPollInterval = 250 * time.Millisecond

func (s *Store) notifyChannel() string { return "dolmen_" + s.catalog }

func (s *Store) announce(ctx context.Context, tx pgx.Tx, ns string) error {
	_, err := tx.Exec(ctx, "SELECT pg_notify($1,$2)", s.notifyChannel(), ns)
	return err
}

type wakeSet struct {
	mu      sync.Mutex
	started bool
	stopped bool
	stop    chan struct{}
	waiters map[string]map[chan struct{}]struct{}
}

func (w *wakeSet) register(ns string, ch chan struct{}) func() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waiters[ns] == nil {
		w.waiters[ns] = map[chan struct{}]struct{}{}
	}
	w.waiters[ns][ch] = struct{}{}
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.waiters[ns], ch)
		if len(w.waiters[ns]) == 0 {
			delete(w.waiters, ns)
		}
	}
}

func (w *wakeSet) signal(ns string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.waiters[ns] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Store) startNotifier() (*wakeSet, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, store.ErrClosed
	}
	if s.wake == nil {
		s.wake = &wakeSet{waiters: map[string]map[chan struct{}]struct{}{}, stop: make(chan struct{})}
	}
	w := s.wake
	s.mu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		w.started = true
		go s.runNotifier(w, w.stop)
	}
	return w, nil
}

func (s *Store) runNotifier(w *wakeSet, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		s.listenOnce(w, stop)
		select {
		case <-stop:
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Store) listenOnce(w *wakeSet, stop chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+ident(s.notifyChannel())); err != nil {
		return
	}
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		w.signal(notification.Payload)
	}
}

func listenCause(err error) error {
	switch {
	case errors.Is(err, store.ErrCursorExpired), errors.Is(err, store.ErrCursorCrossFeed):
		return store.ErrListenAged
	case errors.Is(err, store.ErrNotFound):
		return store.ErrListenLifetimeEnded
	}
	return err
}

type listenSession struct {
	store      *Store
	ns         string
	table      string
	nsGen      [16]byte
	inc        store.Incarnation
	cursor     store.Cursor
	mu         sync.Mutex
	liveAuthz  func(table string) (*store.RowScope, store.Incarnation, bool)
	notify     func(store.ChangeRecord)
	closed     func(error)
	finishOnce sync.Once
	cancelOnce sync.Once
	stopOnce   sync.Once
	firing     atomic.Bool
	stop       chan struct{}
	storeStop  chan struct{}
	wake       chan struct{}
	replayDone chan struct{}
	release    func()
	done       chan struct{}
}

func (l *listenSession) admitted() bool {
	if l.table == "" || l.liveAuthz == nil {
		return true
	}
	_, _, ok := l.liveAuthz(l.table)
	return ok
}

func (l *listenSession) fetch(ctx context.Context) ([]store.ChangeRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.admitted() {
		return nil, store.ErrListenRevoked
	}
	records, next, err := l.store.ChangesSince(ctx, l.ns, l.table, l.cursor, l.nsGen, nil, l.inc, store.Page{})
	if err != nil {
		return nil, listenCause(err)
	}
	l.cursor = next
	return records, nil
}

func (l *listenSession) resume() store.Cursor {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cursor
}

func (l *listenSession) finish(cause error) {
	l.finishOnce.Do(func() {
		if l.closed != nil {
			l.firing.Store(true)
			defer l.firing.Store(false)
			l.closed(cause)
		}
	})
}

func (l *listenSession) halt() {
	l.stopOnce.Do(func() { close(l.stop) })
}

func (l *listenSession) run(ctx context.Context) {
	defer close(l.done)
	select {
	case <-l.replayDone:
	case <-l.stop:
		l.finish(nil)
		return
	case <-l.storeStop:
		l.finish(store.ErrListenLifetimeEnded)
		return
	case <-ctx.Done():
		l.finish(nil)
		return
	}
	ticker := time.NewTicker(listenPollInterval)
	defer ticker.Stop()
	for {
		for {
			records, err := l.fetch(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					l.finish(nil)
				} else {
					l.finish(err)
				}
				return
			}
			if len(records) == 0 {
				break
			}
			for _, record := range records {
				l.notify(record)
			}
		}
		select {
		case <-l.stop:
			l.finish(nil)
			return
		case <-l.storeStop:
			l.finish(store.ErrListenLifetimeEnded)
			return
		case <-ctx.Done():
			l.finish(nil)
			return
		case <-l.wake:
		case <-ticker.C:
		}
	}
}

func (s *Store) listenIncarnation(ctx context.Context, ns, table string) (store.Incarnation, [16]byte, error) {
	var inc store.Incarnation
	var generation [16]byte
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		generation = n.generation
		if table == "" {
			return nil
		}
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		inc = store.Incarnation{NsGen: n.generation, Table: table, DropGen: state.incarnation.DropGen}
		return nil
	})
	return inc, generation, err
}

func (s *Store) Listen(ctx context.Context, ns, table string, from store.Cursor, nsGen [16]byte, liveAuthz func(table string) (*store.RowScope, store.Incarnation, bool), notify func(store.ChangeRecord), closed func(cause error)) (*store.ChangeReplay, func(), error) {
	if notify == nil {
		return nil, nil, invalidf("listen: notify callback is required")
	}
	inc, generation, err := s.listenIncarnation(ctx, ns, table)
	if err != nil {
		return nil, nil, err
	}
	if nsGen == [16]byte{} {
		nsGen = generation
	} else if nsGen != generation {
		return nil, nil, fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, ns)
	}
	w, err := s.startNotifier()
	if err != nil {
		return nil, nil, err
	}
	session := &listenSession{
		store: s, ns: ns, table: table, nsGen: nsGen, inc: inc, cursor: from,
		liveAuthz: liveAuthz, notify: notify, closed: closed,
		stop: make(chan struct{}), storeStop: w.stop,
		wake: make(chan struct{}, 1), replayDone: make(chan struct{}), done: make(chan struct{}),
	}
	if !session.admitted() {
		return nil, nil, store.ErrListenRevoked
	}
	session.release = w.register(ns, session.wake)
	live, cancelLive := context.WithCancel(ctx)
	drained := false

	replay := &store.ChangeReplay{
		Next: func(ctx context.Context) ([]store.ChangeRecord, store.Cursor, bool, error) {
			if drained {
				return nil, session.resume(), true, nil
			}
			batch, err := session.fetch(ctx)
			if err != nil {
				session.finish(err)
				session.halt()
				return nil, "", false, err
			}
			if len(batch) == 0 {
				drained = true
				close(session.replayDone)
				return nil, session.resume(), true, nil
			}
			return batch, session.resume(), false, nil
		},
		Resume: func() store.Cursor { return session.resume() },
	}

	go session.run(live)

	cancel := func() {
		session.cancelOnce.Do(func() {
			session.halt()
			cancelLive()
			if !session.firing.Load() {
				<-session.done
			}
			session.release()
		})
	}
	return replay, cancel, nil
}

func (s *Store) Capabilities() store.EngineCapabilities {
	return store.EngineCapabilities{
		VectorExecution: store.VectorExact,
		ANNRecallBound:  nil,
		Notifications:   true,
		Subscribe:       true,
	}
}
