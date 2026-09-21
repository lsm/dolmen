package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lsm/dolmen/internal/store"
)

const listenPollInterval = 250 * time.Millisecond

func (s *Store) notifyChannel() string { return physicalCandidate("dolmen_"+s.catalog, 0) }

func (s *Store) announce(ctx context.Context, tx pgx.Tx, ns string) {
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return
	}
	if _, err := savepoint.Exec(ctx, "SELECT pg_notify($1,$2)", s.notifyChannel(), ns); err != nil {
		_ = savepoint.Rollback(ctx)
		return
	}
	_ = savepoint.Commit(ctx)
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
	case errors.Is(err, store.ErrCursorCrossFeed):
		return err
	case errors.Is(err, store.ErrCursorExpired):
		return store.ErrListenAged
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrClosed):
		return store.ErrListenLifetimeEnded
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23503" {
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
	drained    bool
	halted     bool
	lastFetch  time.Time
	boundary   int64
	queue      chan store.ChangeRecord
	liveCursor store.Cursor
	stop       chan struct{}
	storeStop  chan struct{}
	wake       chan struct{}
	replayDone chan struct{}
	release    func()
	done       chan struct{}
}

func (l *listenSession) admit(rec store.ChangeRecord) (bool, error) {
	if l.liveAuthz == nil {
		return true, nil
	}
	scope, inc, ok := l.liveAuthz(rec.Table)
	if !ok {
		return false, store.ErrListenRevoked
	}
	if scope != nil {
		if scope.Empty {
			return false, nil
		}
		if rec.Owner == "" {
			return false, store.ErrScopedFeedPredatesLabels
		}
		if scope.Owner != rec.Owner {
			return false, nil
		}
	}
	if inc == (store.Incarnation{}) {
		return true, nil
	}
	if inc.NsGen != rec.Lifetime.NsGen {
		return false, nil
	}
	if l.table != "" && (inc.Table != rec.Lifetime.Table || inc.DropGen != rec.Lifetime.DropGen) {
		return false, nil
	}
	return true, nil
}

func (l *listenSession) admits(table string) error {
	if l.liveAuthz == nil {
		return nil
	}
	if _, _, ok := l.liveAuthz(table); !ok {
		return store.ErrListenRevoked
	}
	return nil
}

func terminalCause(ctx context.Context, err error) (error, bool) {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err, true
	}
	cause := context.Cause(ctx)
	for _, sentinel := range []error{store.ErrListenAged, store.ErrListenRevoked, store.ErrListenLifetimeEnded, store.ErrListenOverflow} {
		if errors.Is(cause, sentinel) {
			return cause, true
		}
	}
	return nil, false
}

func (l *listenSession) guard(ctx context.Context) error {
	if err := l.admits(l.table); err != nil {
		return err
	}
	intact := true
	err := l.store.readOnly(ctx, l.ns, func(tx pgx.Tx, n namespace) error {
		if n.generation != l.nsGen {
			intact = false
			return nil
		}
		if l.table == "" {
			return nil
		}
		state, err := l.store.loadTable(ctx, tx, n, l.table)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				intact = false
				return nil
			}
			return err
		}
		if state.incarnation.DropGen != l.inc.DropGen {
			intact = false
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrListenLifetimeEnded
		}
		return err
	}
	if !intact {
		return store.ErrListenLifetimeEnded
	}
	return nil
}

func (l *listenSession) pending(ctx context.Context, from store.Cursor) bool {
	if from == "" || from == store.CursorBegin {
		return true
	}
	has := true
	err := l.store.readOnly(ctx, l.ns, func(tx pgx.Tx, n namespace) error {
		var position int64
		if err := tx.QueryRow(ctx, "SELECT position FROM "+l.store.relation("cursors")+" WHERE namespace=$1 AND token=$2", n.name, string(from)).Scan(&position); err != nil {
			return err
		}
		stmt := "SELECT EXISTS(SELECT 1 FROM " + l.store.relation("changes") + " WHERE namespace=$1 AND position>$2"
		args := []any{n.name, position}
		if l.table != "" {
			stmt += " AND table_name=$3 AND drop_generation=$4"
			args = append(args, l.table, l.inc.DropGen)
		}
		stmt += ")"
		return tx.QueryRow(ctx, stmt, args...).Scan(&has)
	})
	if err != nil {
		return true
	}
	return has
}

func (l *listenSession) fetch(ctx context.Context, boundary *int64) ([]store.ChangeRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.table != "" {
		if err := l.admits(l.table); err != nil {
			return nil, err
		}
	}
	records, next, err := l.store.changesSinceMode(ctx, l.ns, l.table, l.cursor, l.nsGen, nil, l.inc, store.Page{}, boundary, feedReplay)
	if err != nil {
		return nil, listenCause(err)
	}
	l.cursor = next
	l.lastFetch = l.store.now()
	return records, nil
}

func (l *listenSession) needsRefresh() bool {
	if l.store.changeRetention <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.store.now().Sub(l.lastFetch) >= l.store.changeRetention/4
}

func (l *listenSession) reanchor(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	token, err := l.store.reanchorCursor(ctx, l.ns, l.table, l.liveCursor, l.nsGen, l.inc)
	if err != nil {
		return listenCause(err)
	}
	l.liveCursor = token
	l.lastFetch = l.store.now()
	return nil
}

func (l *listenSession) live() store.Cursor {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.liveCursor
}

func (l *listenSession) fetchLive(ctx context.Context) ([]store.ChangeRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.table != "" {
		if err := l.admits(l.table); err != nil {
			return nil, err
		}
	}
	records, next, err := l.store.changesSinceMode(ctx, l.ns, l.table, l.liveCursor, l.nsGen, nil, l.inc, store.Page{Limit: store.MaxChangesPageLimit}, nil, feedLive)
	if err != nil {
		return nil, listenCause(err)
	}
	l.liveCursor = next
	l.lastFetch = l.store.now()
	return records, nil
}

func (l *listenSession) resume() store.Cursor {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cursor
}

func (l *listenSession) finish(cause error) {
	l.finishOnce.Do(func() {
		if l.closed == nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				slog.Error("listen closed callback panicked; session already ending",
					"namespace", l.ns, "table", l.table, "panic", r)
			}
		}()
		l.firing.Store(true)
		defer l.firing.Store(false)
		l.closed(cause)
	})
}

func (l *listenSession) drain() {
	for {
		select {
		case record := <-l.queue:
			l.deliver(record)
		case <-l.stop:
			return
		}
	}
}

func (l *listenSession) deliver(record store.ChangeRecord) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("listen notify callback panicked; subscription continues",
				"namespace", l.ns, "table", record.Table, "panic", r)
		}
	}()
	l.notify(record)
}

func (l *listenSession) storeClosing() bool {
	select {
	case <-l.storeStop:
		return true
	default:
		return false
	}
}

func (l *listenSession) halt() {
	l.mu.Lock()
	l.halted = true
	l.mu.Unlock()
	l.stopOnce.Do(func() { close(l.stop) })
}

func (l *listenSession) replayStep() (bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.halted, l.drained
}

func (l *listenSession) markDrained() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.drained {
		return false
	}
	l.drained = true
	return true
}

func (l *listenSession) run(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(listenPollInterval)
	defer ticker.Stop()
	for {
		if l.storeClosing() {
			l.finish(store.ErrListenLifetimeEnded)
			return
		}
		if err := l.guard(ctx); err != nil {
			if l.storeClosing() {
				l.finish(store.ErrListenLifetimeEnded)
				return
			}
			if cause, report := terminalCause(ctx, err); report {
				l.finish(cause)
			}
			return
		}
		if l.needsRefresh() {
			if err := l.reanchor(ctx); err != nil {
				if l.storeClosing() {
					l.finish(store.ErrListenLifetimeEnded)
					return
				}
				if cause, report := terminalCause(ctx, err); report {
					l.finish(cause)
				}
				return
			}
		}
		for l.pending(ctx, l.live()) {
			short := false
			for !short {
				records, err := l.fetchLive(ctx)
				if err != nil {
					if l.storeClosing() {
						l.finish(store.ErrListenLifetimeEnded)
						return
					}
					if cause, report := terminalCause(ctx, err); report {
						l.finish(cause)
					}
					return
				}
				for _, record := range records {
					visible, err := l.admit(record)
					if err != nil {
						l.finish(err)
						return
					}
					if !visible {
						continue
					}
					select {
					case l.queue <- record:
					default:
						l.finish(store.ErrListenOverflow)
						return
					}
				}
				short = len(records) < store.MaxChangesPageLimit
			}
		}
		select {
		case <-l.stop:
			return
		case <-l.storeStop:
			l.finish(store.ErrListenLifetimeEnded)
			return
		case <-ctx.Done():
			if cause, report := terminalCause(ctx, ctx.Err()); report {
				l.finish(cause)
			}
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

func (s *Store) validateListenCursor(ctx context.Context, ns, table string, from store.Cursor) error {
	if from == "" || from == store.CursorBegin {
		return nil
	}
	return s.readOnly(ctx, ns, func(tx pgx.Tx, n namespace) error {
		var feed string
		var issued, start time.Time
		err := tx.QueryRow(ctx, "SELECT table_name,issued_at,chain_start FROM "+s.relation("cursors")+" WHERE namespace=$1 AND token=$2", n.name, string(from)).Scan(&feed, &issued, &start)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrCursorExpired
		}
		if err != nil {
			return err
		}
		if feed != table {
			return store.ErrCursorCrossFeed
		}
		now := s.now()
		if s.changeRetention > 0 && (now.After(issued.Add(s.changeRetention)) || now.After(start.Add(2*s.changeRetention))) {
			return store.ErrCursorExpired
		}
		return nil
	})
}

func (s *Store) Listen(ctx context.Context, ns, table string, from store.Cursor, nsGen [16]byte, liveAuthz func(table string) (*store.RowScope, store.Incarnation, bool), notify func(store.ChangeRecord), closed func(cause error)) (*store.ChangeReplay, func(), error) {
	if notify == nil {
		return nil, nil, invalidf("listen: notify callback is required")
	}
	inc, generation, err := s.listenIncarnation(ctx, ns, table)
	if err != nil {
		return nil, nil, err
	}
	if err := s.validateListenCursor(ctx, ns, table, from); err != nil {
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
	anchored, liveAnchor, head, err := s.anchorListen(ctx, ns, table, from, nsGen, inc)
	if err != nil {
		return nil, nil, listenCause(err)
	}
	session := &listenSession{
		store: s, ns: ns, table: table, nsGen: nsGen, inc: inc, cursor: anchored,
		liveAuthz: liveAuthz, notify: notify, closed: closed,
		stop: make(chan struct{}), storeStop: w.stop,
		wake: make(chan struct{}, 1), replayDone: make(chan struct{}), done: make(chan struct{}),
		lastFetch: s.now(), boundary: head,
		queue: make(chan store.ChangeRecord, store.ListenQueueBound), liveCursor: liveAnchor,
	}
	if table != "" {
		if err := session.admits(table); err != nil {
			return nil, nil, err
		}
		if liveAuthz != nil {
			if scope, _, ok := liveAuthz(table); ok && scope != nil {
				stale, serr := s.unlabeledBacklog(ctx, ns, table, from)
				if serr != nil {
					return nil, nil, listenCause(serr)
				}
				if stale {
					return nil, nil, store.ErrScopedFeedPredatesLabels
				}
			}
		}
	}
	session.release = w.register(ns, session.wake)
	live, cancelLive := context.WithCancel(ctx)

	replay := &store.ChangeReplay{
		Next: func(ctx context.Context) ([]store.ChangeRecord, store.Cursor, bool, error) {
			halted, drained := session.replayStep()
			if halted {
				return nil, session.resume(), true, store.ErrClosed
			}
			if drained {
				return nil, session.resume(), true, nil
			}
			batch, err := session.fetch(ctx, &session.boundary)
			if err != nil {
				if cause, report := terminalCause(ctx, err); report {
					session.finish(cause)
					session.halt()
				}
				return nil, "", false, err
			}
			if len(batch) == 0 {
				if session.markDrained() {
					close(session.replayDone)
				}
				return nil, session.resume(), true, nil
			}
			admitted := batch[:0]
			for _, record := range batch {
				visible, err := session.admit(record)
				if err != nil {
					session.finish(err)
					session.halt()
					return nil, "", false, err
				}
				if visible {
					admitted = append(admitted, record)
				}
			}
			batch = admitted
			return batch, session.resume(), false, nil
		},
		Resume: func() store.Cursor { return session.resume() },
	}

	go session.run(live)
	go session.drain()

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
