package lakehouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

const listenPollInterval = 250 * time.Millisecond

type wakeSet struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
}

func (w *wakeSet) register(ns string, ch chan struct{}) func() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waiters == nil {
		w.waiters = map[string]map[chan struct{}]struct{}{}
	}
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

func listenCause(err error) error {
	switch {
	case errors.Is(err, store.ErrCursorCrossFeed):
		return err
	case errors.Is(err, store.ErrCursorExpired):
		return store.ErrListenAged
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrClosed):
		return store.ErrListenLifetimeEnded
	}
	return err
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

func (l *listenSession) guard(ctx context.Context) error {
	if err := l.admits(l.table); err != nil {
		return err
	}
	intact := true
	err := l.store.withNamespace(ctx, l.ns, func(n *namespace) error {
		if n.generation != l.nsGen {
			intact = false
			return nil
		}
		if l.table == "" {
			return nil
		}
		state, err := loadTable(ctx, n, l.ns, l.table)
		if errors.Is(err, store.ErrNotFound) {
			intact = false
			return nil
		}
		if err != nil {
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
	err := l.store.withNamespace(ctx, l.ns, func(n *namespace) error {
		var position int64
		if err := n.db.QueryRowContext(ctx, `SELECT position FROM _dolmen_lakehouse_cursors WHERE token = ?`, string(from)).Scan(&position); err != nil {
			return err
		}
		stmt := `SELECT EXISTS(SELECT 1 FROM _dolmen_lakehouse_changes WHERE seq > ?`
		args := []any{position}
		if l.table != "" {
			stmt += ` AND table_name = ? AND generation = ?`
			args = append(args, l.table, l.inc.DropGen)
		}
		return n.db.QueryRowContext(ctx, stmt+`)`, args...).Scan(&has)
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
	records, next, err := l.store.changesSinceMode(ctx, l.ns, l.table, l.cursor, l.nsGen, nil, l.inc, store.Page{}, boundary)
	if err != nil {
		return nil, listenCause(err)
	}
	l.cursor = next
	l.lastFetch = time.Now()
	return records, nil
}

func (l *listenSession) needsRefresh() bool {
	if l.store.retention <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Since(l.lastFetch) >= l.store.retention/4
}

func (l *listenSession) reanchor(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	token, err := l.store.reanchorCursor(ctx, l.ns, l.table, l.liveCursor, l.nsGen, l.inc)
	if err != nil {
		return listenCause(err)
	}
	l.liveCursor = token
	l.lastFetch = time.Now()
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
	records, next, err := l.store.changesSinceMode(ctx, l.ns, l.table, l.liveCursor, l.nsGen, nil, l.inc, store.Page{Limit: store.MaxChangesPageLimit}, nil)
	if err != nil {
		return nil, listenCause(err)
	}
	l.liveCursor = next
	l.lastFetch = time.Now()
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
				slog.Error("listen closed callback panicked; session already ending", "namespace", l.ns, "table", l.table, "panic", r)
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
			slog.Error("listen notify callback panicked; subscription continues", "namespace", l.ns, "table", record.Table, "panic", r)
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

func (l *listenSession) markDrained() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.drained = true
}

func (l *listenSession) fail(ctx context.Context, err error) {
	if l.storeClosing() {
		l.finish(store.ErrListenLifetimeEnded)
		return
	}
	if cause, report := terminalCause(ctx, err); report {
		l.finish(cause)
	}
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
			l.fail(ctx, err)
			return
		}
		if l.needsRefresh() {
			if err := l.reanchor(ctx); err != nil {
				l.fail(ctx, err)
				return
			}
		}
		for l.pending(ctx, l.live()) {
			short := false
			for !short {
				records, err := l.fetchLive(ctx)
				if err != nil {
					l.fail(ctx, err)
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

func (s *Store) beginPosition(ctx context.Context, q *sql.Tx, now time.Time) (int64, bool, error) {
	var first sql.NullInt64
	stmt := `SELECT min(seq) FROM _dolmen_lakehouse_changes`
	var args []any
	if s.retention > 0 {
		stmt += ` WHERE at >= ?`
		args = append(args, now.Add(-s.retention).UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	if err := q.QueryRowContext(ctx, stmt, args...).Scan(&first); err != nil {
		return 0, false, err
	}
	return first.Int64 - 1, first.Valid, nil
}

func (s *Store) feedDrop(ctx context.Context, n *namespace, ns, table string, inc store.Incarnation) (int64, error) {
	if table == "" {
		return 0, nil
	}
	state, err := loadTable(ctx, n, ns, table)
	if err != nil {
		return 0, err
	}
	if err := checkExpected(state, inc, false); err != nil {
		return 0, err
	}
	return state.incarnation.DropGen, nil
}

func (s *Store) anchorListen(ctx context.Context, ns, table string, from store.Cursor, expected [16]byte, inc store.Incarnation) (store.Cursor, store.Cursor, int64, error) {
	var replay, live store.Cursor
	var head int64
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		if expected != n.generation {
			return fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, ns)
		}
		now := time.Now()
		drop, err := s.feedDrop(ctx, n, ns, table, inc)
		if err != nil {
			return err
		}
		tx, err := n.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if head, err = changeHead(ctx, tx); err != nil {
			return err
		}
		state := cursorState{position: head, origin: head, start: now.UnixNano(), table: table, drop: drop}
		if live, err = mintCursor(ctx, tx, state, now); err != nil {
			return err
		}
		if from != "" && from != store.CursorBegin {
			replay = from
			return tx.Commit()
		}
		if from == store.CursorBegin {
			first, ok, err := s.beginPosition(ctx, tx, now)
			if err != nil {
				return err
			}
			if ok {
				state.position, state.origin = first, first
			}
		}
		if replay, err = mintCursor(ctx, tx, state, now); err != nil {
			return err
		}
		return tx.Commit()
	})
	return replay, live, head, err
}

func (s *Store) reanchorCursor(ctx context.Context, ns, table string, from store.Cursor, expected [16]byte, inc store.Incarnation) (store.Cursor, error) {
	if from == "" || from == store.CursorBegin {
		return from, nil
	}
	token := from
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		if expected != n.generation {
			return fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, ns)
		}
		now := time.Now()
		drop, err := s.feedDrop(ctx, n, ns, table, inc)
		if err != nil {
			return err
		}
		tx, err := n.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		state, err := s.resolveCursor(ctx, tx, from, table, drop, now)
		if err != nil {
			return err
		}
		state.origin = state.position
		state.start = now.UnixNano()
		if token, err = mintCursor(ctx, tx, state, now); err != nil {
			return err
		}
		if err := s.pruneChanges(ctx, tx, now); err != nil {
			return err
		}
		return tx.Commit()
	})
	return token, err
}

func (s *Store) unlabeledBacklog(ctx context.Context, ns, table string, from store.Cursor) (bool, error) {
	stale := false
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		drop, err := s.feedDrop(ctx, n, ns, table, store.Incarnation{})
		if err != nil {
			return err
		}
		now := time.Now()
		tx, err := n.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		head, err := changeHead(ctx, tx)
		if err != nil {
			return err
		}
		position := head
		switch {
		case from == "":
		case from == store.CursorBegin:
			if first, ok, err := s.beginPosition(ctx, tx, now); err != nil {
				return err
			} else if ok {
				position = first
			}
		default:
			state, err := s.resolveCursor(ctx, tx, from, table, drop, now)
			if err != nil {
				return err
			}
			position = state.position
		}
		if position >= head {
			return tx.Commit()
		}
		var one int
		switch qerr := tx.QueryRowContext(ctx, `SELECT 1 FROM _dolmen_lakehouse_changes WHERE seq > ? AND seq <= ? AND owner IS NULL AND table_name = ? AND generation = ? LIMIT 1`, position, head, table, drop).Scan(&one); {
		case qerr == nil:
			stale = true
		case !errors.Is(qerr, sql.ErrNoRows):
			return qerr
		}
		return tx.Commit()
	})
	return stale, err
}

func (s *Store) validateListenCursor(ctx context.Context, ns, table string, from store.Cursor) error {
	if from == "" || from == store.CursorBegin {
		return nil
	}
	return s.withNamespace(ctx, ns, func(n *namespace) error {
		var feed string
		var issued, start int64
		err := n.db.QueryRowContext(ctx, `SELECT table_name, issued_at, chain_start FROM _dolmen_lakehouse_cursors WHERE token = ?`, string(from)).Scan(&feed, &issued, &start)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrCursorExpired
		}
		if err != nil {
			return err
		}
		if feed != table {
			return store.ErrCursorCrossFeed
		}
		now := time.Now().UnixNano()
		if s.retention > 0 && (now > issued+int64(s.retention) || now > start+2*int64(s.retention)) {
			return store.ErrCursorExpired
		}
		return nil
	})
}

func (s *Store) Listen(ctx context.Context, ns, table string, from store.Cursor, nsGen [16]byte, liveAuthz func(table string) (*store.RowScope, store.Incarnation, bool), notify func(store.ChangeRecord), closed func(cause error)) (*store.ChangeReplay, func(), error) {
	if notify == nil {
		return nil, nil, invalidf("listen: notify callback is required")
	}
	var inc store.Incarnation
	var generation [16]byte
	if err := s.withNamespace(ctx, ns, func(n *namespace) error {
		generation = n.generation
		if table == "" {
			return nil
		}
		state, err := loadTable(ctx, n, ns, table)
		if err != nil {
			return err
		}
		inc = store.Incarnation{NsGen: n.generation, Table: table, DropGen: state.incarnation.DropGen}
		return nil
	}); err != nil {
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
	anchored, liveAnchor, head, err := s.anchorListen(ctx, ns, table, from, nsGen, inc)
	if err != nil {
		return nil, nil, listenCause(err)
	}
	session := &listenSession{
		store: s, ns: ns, table: table, nsGen: nsGen, inc: inc, cursor: anchored,
		liveAuthz: liveAuthz, notify: notify, closed: closed,
		stop: make(chan struct{}), storeStop: s.stopping,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		lastFetch: time.Now(), boundary: head,
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
	session.release = s.wake.register(ns, session.wake)
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
				session.markDrained()
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
			return admitted, session.resume(), false, nil
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
