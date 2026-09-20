package postgres

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
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
	stop    chan struct{}
	waiters map[string]map[chan struct{}]struct{}
}

func (s *Store) wakes() *wakeSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = &wakeSet{waiters: map[string]map[chan struct{}]struct{}{}, stop: make(chan struct{})}
	}
	return s.wake
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

func (s *Store) startNotifier() {
	w := s.wakes()
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	w.started = true
	stop := w.stop
	w.mu.Unlock()
	go s.runNotifier(w, stop)
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
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+ident(s.notifyChannel())); err != nil {
		return
	}
	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return
		}
		w.signal(notification.Payload)
	}
}

type listenSession struct {
	store     *Store
	ns        string
	table     string
	nsGen     [16]byte
	cursor    store.Cursor
	mu        sync.Mutex
	liveAuthz func(table string) (*store.RowScope, store.Incarnation, bool)
	notify    func(store.ChangeRecord)
	closed    func(error)
	once      sync.Once
	stop      chan struct{}
	wake      chan struct{}
	release   func()
	done      chan struct{}
}

func (l *listenSession) incarnation() (store.Incarnation, bool) {
	if l.table == "" || l.liveAuthz == nil {
		return store.Incarnation{NsGen: l.nsGen}, true
	}
	_, inc, ok := l.liveAuthz(l.table)
	return inc, ok
}

func (l *listenSession) fetch(ctx context.Context) ([]store.ChangeRecord, error) {
	inc, ok := l.incarnation()
	if !ok {
		return nil, derr.New(derr.Forbidden, "subscription is no longer admitted to %s", l.table)
	}
	l.mu.Lock()
	from := l.cursor
	l.mu.Unlock()
	records, next, err := l.store.ChangesSince(ctx, l.ns, l.table, from, l.nsGen, nil, inc, store.Page{})
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.cursor = next
	l.mu.Unlock()
	return records, nil
}

func (l *listenSession) finish(cause error) {
	l.once.Do(func() {
		if l.closed != nil {
			l.closed(cause)
		}
	})
}

func (l *listenSession) run(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(listenPollInterval)
	defer ticker.Stop()
	for {
		for {
			records, err := l.fetch(ctx)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					l.finish(err)
				} else {
					l.finish(nil)
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
		case <-ctx.Done():
			l.finish(nil)
			return
		case <-l.wake:
		case <-ticker.C:
		}
	}
}

func (s *Store) Listen(ctx context.Context, ns, table string, from store.Cursor, nsGen [16]byte, liveAuthz func(table string) (*store.RowScope, store.Incarnation, bool), notify func(store.ChangeRecord), closed func(cause error)) (*store.ChangeReplay, func(), error) {
	if notify == nil {
		return nil, nil, invalidf("listen: notify callback is required")
	}
	session := &listenSession{
		store: s, ns: ns, table: table, nsGen: nsGen, cursor: from,
		liveAuthz: liveAuthz, notify: notify, closed: closed,
		stop: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	inc, ok := session.incarnation()
	if !ok {
		return nil, nil, derr.New(derr.Forbidden, "subscription is not admitted to %s", table)
	}
	records, next, err := s.ChangesSince(ctx, ns, table, from, nsGen, nil, inc, store.Page{})
	if err != nil {
		return nil, nil, err
	}
	session.cursor = next
	pending := records
	drained := false

	replay := &store.ChangeReplay{
		Next: func(ctx context.Context) ([]store.ChangeRecord, store.Cursor, bool, error) {
			if drained {
				session.mu.Lock()
				resume := session.cursor
				session.mu.Unlock()
				return nil, resume, true, nil
			}
			if len(pending) > 0 {
				batch := pending
				pending = nil
				session.mu.Lock()
				resume := session.cursor
				session.mu.Unlock()
				return batch, resume, false, nil
			}
			batch, err := session.fetch(ctx)
			if err != nil {
				return nil, "", false, err
			}
			session.mu.Lock()
			resume := session.cursor
			session.mu.Unlock()
			if len(batch) == 0 {
				drained = true
				return nil, resume, true, nil
			}
			return batch, resume, false, nil
		},
		Resume: func() store.Cursor {
			session.mu.Lock()
			defer session.mu.Unlock()
			return session.cursor
		},
	}

	s.startNotifier()
	session.release = s.wakes().register(ns, session.wake)
	live, cancelLive := context.WithCancel(context.WithoutCancel(ctx))
	go session.run(live)

	cancel := func() {
		close(session.stop)
		cancelLive()
		<-session.done
		session.release()
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
