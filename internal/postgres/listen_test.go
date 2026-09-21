package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func drainReplay(t *testing.T, ctx context.Context, replay *store.ChangeReplay) []store.ChangeRecord {
	t.Helper()
	out := []store.ChangeRecord{}
	for {
		records, _, done, err := replay.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, records...)
		if done {
			return out
		}
	}
}

func collectLive(t *testing.T, live <-chan store.ChangeRecord, want int) []store.ChangeRecord {
	t.Helper()
	out := []store.ChangeRecord{}
	deadline := time.After(20 * time.Second)
	for len(out) < want {
		select {
		case rec := <-live:
			out = append(out, rec)
		case <-deadline:
			t.Fatalf("timed out after %d of %d live changes", len(out), want)
		}
	}
	return out
}

func listenSeed(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresListenReplaysThenStreamsLive(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}, {"body": "two"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	live := make(chan store.ChangeRecord, 32)
	replay, cancel, err := s.Listen(ctx, "app", "notes", store.CursorBegin, [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	replayed := drainReplay(t, ctx, replay)
	if len(replayed) != 2 {
		t.Fatalf("replayed %d changes, want 2", len(replayed))
	}
	for _, rec := range replayed {
		if rec.Table != "notes" || rec.Kind != store.ChangeInsert || rec.Cursor == "" {
			t.Fatalf("replay record: %+v", rec)
		}
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "three"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	got := collectLive(t, live, 1)
	if got[0].Table != "notes" || got[0].Kind != store.ChangeInsert {
		t.Fatalf("live record: %+v", got[0])
	}
	if got[0].RowID != 3 {
		t.Fatalf("live row id: %+v", got[0])
	}
}

func TestPostgresListenFromHeadSkipsHistory(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "old"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	live := make(chan store.ChangeRecord, 32)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if replayed := drainReplay(t, ctx, replay); len(replayed) != 0 {
		t.Fatalf("head cursor replayed %d changes", len(replayed))
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "new"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	got := collectLive(t, live, 1)
	if got[0].RowID != 2 {
		t.Fatalf("live record: %+v", got[0])
	}
}

func TestPostgresListenCrossesStoreInstances(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	listenSeed(t, s, ctx)
	writer, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	live := make(chan store.ChangeRecord, 32)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	if _, err := writer.Insert(ctx, "app", "notes", []map[string]any{{"body": "from another instance"}, {"body": "and another"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	got := collectLive(t, live, 2)
	for _, rec := range got {
		if rec.Table != "notes" || rec.Kind != store.ChangeInsert {
			t.Fatalf("cross-instance record: %+v", rec)
		}
	}
	if got[0].RowID == got[1].RowID {
		t.Fatalf("duplicate rows: %+v", got)
	}
}

func TestPostgresListenSurvivesWithoutNotifications(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	listenSeed(t, s, ctx)
	live := make(chan store.ChangeRecord, 32)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	time.Sleep(4 * listenPollInterval)
	s.wake.mu.Lock()
	waiters := s.wake.waiters
	s.wake.waiters = map[string]map[chan struct{}]struct{}{}
	s.wake.mu.Unlock()
	defer func() {
		s.wake.mu.Lock()
		s.wake.waiters = waiters
		s.wake.mu.Unlock()
	}()
	select {
	case rec := <-live:
		t.Fatalf("unexpected change before the write: %+v", rec)
	default:
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "polled"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	got := collectLive(t, live, 1)
	if got[0].RowID != 1 {
		t.Fatalf("durable poll record: %+v", got[0])
	}
}

func TestPostgresListenClosesWhenAdmissionRevoked(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	var admitted atomic.Bool
	admitted.Store(true)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, func(string) (*store.RowScope, store.Incarnation, bool) {
		return nil, store.Incarnation{}, admitted.Load()
	}, func(store.ChangeRecord) {}, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	admitted.Store(false)
	select {
	case cause := <-closedWith:
		if !errors.Is(cause, store.ErrListenRevoked) {
			t.Fatalf("revoked admission cause = %v, want ErrListenRevoked", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("revoked admission never closed the subscription")
	}
}

func TestPostgresListenCancelStopsDelivery(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	delivered := make(chan store.ChangeRecord, 32)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { delivered <- rec }, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	drainReplay(t, ctx, replay)
	cancel()
	select {
	case cause := <-closedWith:
		t.Fatalf("plain cancellation reported a cause (%v); the transports treat a nil cause as a panic", cause)
	case <-time.After(2 * time.Second):
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "after cancel"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-delivered:
		t.Fatalf("delivered after cancel: %+v", rec)
	case <-time.After(2 * time.Second):
	}
}

func TestPostgresListenRequiresNotify(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	if _, _, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, nil, nil); err == nil {
		t.Fatal("missing notify callback accepted")
	}
}

func TestPostgresCapabilities(t *testing.T) {
	s := openTest(t, testConfig(t))
	caps := s.Capabilities()
	if caps.VectorExecution != store.VectorExact || !caps.Notifications || !caps.Subscribe || caps.ANNRecallBound != nil {
		t.Fatalf("capabilities: %+v", caps)
	}
}

func TestPostgresListenDeliversEachChangeOnce(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	for i := 0; i < 5; i++ {
		if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "before"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	live := make(chan store.ChangeRecord, 64)
	replay, cancel, err := s.Listen(ctx, "app", "notes", store.CursorBegin, [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	writing := make(chan struct{})
	go func() {
		defer close(writing)
		for i := 0; i < 5; i++ {
			if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "during"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				return
			}
		}
	}()
	replayed := drainReplay(t, ctx, replay)
	<-writing
	seen := map[int64]int{}
	for _, rec := range replayed {
		seen[rec.RowID]++
	}
	for len(seen) < 10 {
		select {
		case rec := <-live:
			seen[rec.RowID]++
		case <-time.After(20 * time.Second):
			t.Fatalf("only %d of 10 changes arrived: %+v", len(seen), seen)
		}
	}
	select {
	case rec := <-live:
		seen[rec.RowID]++
	case <-time.After(2 * time.Second):
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("row %d delivered %d times across the replay/live boundary: %+v", id, count, seen)
		}
	}
	if len(seen) != 10 {
		t.Fatalf("delivered %d distinct rows, want 10", len(seen))
	}
}

func TestPostgresListenCancelIsIdempotent(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(store.ChangeRecord) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	drainReplay(t, ctx, replay)
	cancel()
	cancel()
	cancel()
}

func TestPostgresListenEndsWhenTableLifetimeEnds(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(store.ChangeRecord) {}, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	if err := s.DropTable(ctx, "app", "notes", store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-closedWith:
		if !errors.Is(cause, store.ErrListenLifetimeEnded) {
			t.Fatalf("cause = %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a recreated table did not end the subscription")
	}
}

func TestPostgresListenDoesNotStarveASingleConnectionPool(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxConns = 1
	s := openTest(t, cfg)
	ctx := t.Context()
	listenSeed(t, s, ctx)
	live := make(chan store.ChangeRecord, 16)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	writeCtx, writeCancel := context.WithTimeout(ctx, 20*time.Second)
	defer writeCancel()
	if _, err := s.Insert(writeCtx, "app", "notes", []map[string]any{{"body": "single slot"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("write starved by the LISTEN connection: %v", err)
	}
	got := collectLive(t, live, 1)
	if got[0].RowID != 1 {
		t.Fatalf("record: %+v", got[0])
	}
}

func TestPostgresListenCloseStopsNotifier(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	listenSeed(t, s, ctx)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(store.ChangeRecord) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	drainReplay(t, ctx, replay)
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Close hung with a notifier running")
	}
	s.wake.mu.Lock()
	stopped := s.wake.stopped
	s.wake.mu.Unlock()
	if !stopped {
		t.Fatal("Close left the notifier running")
	}
	if _, _, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(store.ChangeRecord) {}, nil); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Listen after Close: %v", err)
	}
}

func TestPostgresListenReplayFailureFiresClosed(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", store.CursorBegin, [16]byte{}, nil, func(store.ChangeRecord) {}, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := s.DropTable(ctx, "app", "notes", store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := replay.Next(ctx); err == nil {
		t.Fatal("replay against a dropped table succeeded")
	} else if !errors.Is(err, store.ErrListenLifetimeEnded) {
		t.Fatalf("replay error = %v, want ErrListenLifetimeEnded", err)
	}
	select {
	case cause := <-closedWith:
		if !errors.Is(cause, store.ErrListenLifetimeEnded) {
			t.Fatalf("closed cause = %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a terminal replay error never fired closed")
	}
}

func TestPostgresListenCancelFromClosedCallbackDoesNotDeadlock(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	var cancel func()
	ready := make(chan struct{})
	returned := make(chan struct{})
	admitted := atomic.Bool{}
	admitted.Store(true)
	replay, c, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, func(string) (*store.RowScope, store.Incarnation, bool) {
		return nil, store.Incarnation{}, admitted.Load()
	}, func(store.ChangeRecord) {}, func(error) {
		<-ready
		cancel()
		close(returned)
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel = c
	defer cancel()
	drainReplay(t, ctx, replay)
	close(ready)
	admitted.Store(false)
	select {
	case <-returned:
	case <-time.After(20 * time.Second):
		t.Fatal("cancel called from the closed callback deadlocked")
	}
}

func TestPostgresListenMapsAgedContextCause(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	aged, expire := context.WithCancelCause(ctx)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(aged, "app", "notes", "", [16]byte{}, nil, func(store.ChangeRecord) {}, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	expire(store.ErrListenAged)
	select {
	case cause := <-closedWith:
		if !errors.Is(cause, store.ErrListenAged) {
			t.Fatalf("cause = %v, want ErrListenAged", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("an aged context never ended the subscription")
	}
}

func TestPostgresListenDeliversOnlyTheScopesOwnRows(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}},
		store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"bob", "alice", "bob"} {
		if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "from " + who}},
			store.WriteOpts{Owner: who}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	replay, cancel, err := s.Listen(ctx, "app", "notes", store.CursorBegin, [16]byte{},
		func(string) (*store.RowScope, store.Incarnation, bool) {
			return &store.RowScope{Owner: "alice"}, store.Incarnation{}, true
		}, func(store.ChangeRecord) {}, nil)
	if err != nil {
		t.Fatalf("a row scope must be admitted now that change records carry the owner: %v", err)
	}
	defer cancel()
	records := drainReplay(t, ctx, replay)
	if len(records) != 1 {
		t.Fatalf("alice wrote one of the three rows and must replay exactly that: %v", records)
	}
	if records[0].RowID != 2 {
		t.Fatalf("the replayed record is not alice's: %v", records[0])
	}
}

func TestPostgresListenDeliversNothingToAnEmptyScope(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}},
		store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "from alice"}},
		store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	replay, cancel, err := s.Listen(ctx, "app", "notes", store.CursorBegin, [16]byte{},
		func(string) (*store.RowScope, store.Incarnation, bool) {
			return &store.RowScope{Empty: true}, store.Incarnation{}, true
		}, func(store.ChangeRecord) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if records := drainReplay(t, ctx, replay); len(records) != 0 {
		t.Fatalf("an empty scope sees no row, so it must replay nothing: %v", records)
	}
}

func TestPostgresListenRechecksNamespaceFeedAdmission(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	var admitted atomic.Bool
	admitted.Store(true)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "", "", [16]byte{}, func(string) (*store.RowScope, store.Incarnation, bool) {
		return nil, store.Incarnation{}, admitted.Load()
	}, func(store.ChangeRecord) {}, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	admitted.Store(false)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "after revoke"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-closedWith:
		if !errors.Is(cause, store.ErrListenRevoked) {
			t.Fatalf("cause = %v, want ErrListenRevoked", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a namespace-wide feed never re-checked admission")
	}
}

func TestPostgresListenIdlePollDoesNotWrite(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(store.ChangeRecord) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	count := func() int64 {
		var n int64
		if err := s.read(ctx, "app", func(tx pgx.Tx, _ namespace) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM "+s.relation("cursors")).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	time.Sleep(8 * listenPollInterval)
	if after := count(); after != before {
		t.Fatalf("idle polling minted %d cursors in %v", after-before, 8*listenPollInterval)
	}
}

func TestPostgresNotifyChannelFitsIdentifierLimit(t *testing.T) {
	for _, catalog := range []string{"dolmen_catalog", strings.Repeat("c", 63), "c" + strings.Repeat("x", 62)} {
		s := &Store{catalog: catalog}
		channel := s.notifyChannel()
		if len(channel) > 63 {
			t.Fatalf("catalog %d chars yields a %d-byte channel %q", len(catalog), len(channel), channel)
		}
		if channel != (&Store{catalog: catalog}).notifyChannel() {
			t.Fatalf("channel for catalog %q is not deterministic", catalog)
		}
	}
	short := (&Store{catalog: "app"}).notifyChannel()
	long := (&Store{catalog: strings.Repeat("c", 63)}).notifyChannel()
	if short == long {
		t.Fatal("distinct catalogs collided on one channel")
	}
}

func TestPostgresListenWorksWithALongCatalogName(t *testing.T) {
	cfg := testConfig(t)
	cfg.Catalog = cfg.Catalog + "_" + strings.Repeat("z", 63-len(cfg.Catalog)-1)
	if len(cfg.Catalog) != 63 {
		t.Fatalf("catalog is %d chars", len(cfg.Catalog))
	}
	s := openTest(t, cfg)
	ctx := t.Context()
	listenSeed(t, s, ctx)
	live := make(chan store.ChangeRecord, 16)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "long catalog"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("write failed under a 63-character catalog: %v", err)
	}
	got := collectLive(t, live, 1)
	if got[0].RowID != 1 {
		t.Fatalf("record: %+v", got[0])
	}
}

func TestPostgresAnnounceFailureDoesNotAbortWrites(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	if err := s.write(ctx, "app", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		if _, err := tx.Exec(ctx, "SELECT pg_notify($1,$2)", strings.Repeat("q", 64), n.name); err == nil {
			return errors.New("expected an oversized channel to be rejected")
		}
		return nil
	}); err == nil {
		t.Fatal("expected the poisoned transaction to surface")
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "after"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, "app", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		s.announce(ctx, tx, n.name)
		var alive int
		return tx.QueryRow(ctx, "SELECT 1").Scan(&alive)
	}); err != nil {
		t.Fatalf("announce left the transaction unusable: %v", err)
	}
}

func TestPostgresListenPanickingClosedCallbackDoesNotCrash(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	var admitted atomic.Bool
	admitted.Store(true)
	panicked := make(chan struct{})
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, func(string) (*store.RowScope, store.Incarnation, bool) {
		return nil, store.Incarnation{}, admitted.Load()
	}, func(store.ChangeRecord) {}, func(error) {
		close(panicked)
		panic("closed callback exploded")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	admitted.Store(false)
	select {
	case <-panicked:
	case <-time.After(20 * time.Second):
		t.Fatal("closed callback never fired")
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "still alive"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("store unusable after a panicking callback: %v", err)
	}
}

func TestPostgresListenCloseReportsLifetimeEnded(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	listenSeed(t, s, ctx)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(store.ChangeRecord) {}, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-closedWith:
		if !errors.Is(cause, store.ErrListenLifetimeEnded) {
			t.Fatalf("closing store reported %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("closing the store never ended the subscription")
	}
}

func TestPostgresListenRejectsCrossFeedCursorAtListen(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, tableCursor, err := s.ChangesSince(ctx, "app", "notes", store.CursorBegin, [16]byte{}, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Listen(ctx, "app", "", tableCursor, [16]byte{}, nil, func(store.ChangeRecord) {}, nil)
	if !errors.Is(err, store.ErrCursorCrossFeed) {
		t.Fatalf("cross-feed cursor reported %v, want ErrCursorCrossFeed", err)
	}
	if errors.Is(err, store.ErrListenAged) {
		t.Fatal("cross-feed cursor was reported as an age bound")
	}
	if _, _, err := s.Listen(ctx, "app", "notes", store.Cursor("deadbeefdeadbeefdeadbeefdeadbeef"), [16]byte{}, nil, func(store.ChangeRecord) {}, nil); !errors.Is(err, store.ErrCursorExpired) {
		t.Fatalf("unknown cursor reported %v, want ErrCursorExpired", err)
	}
}

func TestPostgresListenReplayRejectsCallsAfterCancel(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}, {"body": "two"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	replay, cancel, err := s.Listen(ctx, "app", "notes", store.CursorBegin, [16]byte{}, nil, func(store.ChangeRecord) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, _, _, err := replay.Next(ctx); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Next after cancel returned %v, want ErrClosed", err)
	}
}

func TestPostgresListenConcurrentReplayNextIsSafe(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(store.ChangeRecord) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, _, _, err := replay.Next(ctx); err != nil && !errors.Is(err, store.ErrClosed) {
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestPostgresListenSurvivesPastTwiceChangeRetention(t *testing.T) {
	cfg := testConfig(t)
	retention := 2 * time.Second
	cfg.ChangeRetention = &retention
	s := openTest(t, cfg)
	ctx := t.Context()
	listenSeed(t, s, ctx)
	live := make(chan store.ChangeRecord, 128)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	deadline := time.Now().Add(5 * retention)
	written := 0
	for time.Now().Before(deadline) {
		if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "tick"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
		written++
		select {
		case cause := <-closedWith:
			t.Fatalf("subscription ended after %v with %v", time.Since(deadline.Add(-5*retention)), cause)
		case <-time.After(retention / 4):
		}
	}
	delivered := 0
	for {
		select {
		case <-live:
			delivered++
			continue
		case cause := <-closedWith:
			t.Fatalf("subscription ended with %v", cause)
		case <-time.After(2 * listenPollInterval):
		}
		break
	}
	if delivered == 0 {
		t.Fatalf("nothing delivered across %d writes", written)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "final"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-live:
	case cause := <-closedWith:
		t.Fatalf("subscription ended with %v", cause)
	case <-time.After(20 * time.Second):
		t.Fatal("subscription stopped delivering past twice the retention window")
	}
}

func TestPostgresListenPanickingNotifyDoesNotCrash(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	var seen atomic.Int64
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(store.ChangeRecord) {
		seen.Add(1)
		panic("notify callback exploded")
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}, {"body": "two"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(20 * time.Second)
	for seen.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("a panicking notify stopped delivery after %d records", seen.Load())
		case <-time.After(listenPollInterval):
		}
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "three"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("store unusable after a panicking notify: %v", err)
	}
}

func TestPostgresListenNamespaceReplayRechecksAdmission(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}, {"body": "two"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Bool
	admitted.Store(false)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "", store.CursorBegin, [16]byte{}, func(string) (*store.RowScope, store.Incarnation, bool) {
		return nil, store.Incarnation{}, admitted.Load()
	}, func(store.ChangeRecord) {}, func(cause error) { closedWith <- cause })
	if err != nil {
		if !errors.Is(err, store.ErrListenRevoked) {
			t.Fatalf("Listen with revoked admission: %v", err)
		}
		return
	}
	defer cancel()
	if _, _, _, err := replay.Next(ctx); !errors.Is(err, store.ErrListenRevoked) {
		t.Fatalf("namespace replay delivered under revoked admission: %v", err)
	}
	select {
	case cause := <-closedWith:
		if !errors.Is(cause, store.ErrListenRevoked) {
			t.Fatalf("closed cause = %v, want ErrListenRevoked", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("revoked admission during replay never fired closed")
	}
}

func TestPostgresListenDeliversWritesRacingTheFirstNext(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	live := make(chan store.ChangeRecord, 32)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "between listen and next"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, rec := range drainReplay(t, ctx, replay) {
		seen[rec.RowID] = true
	}
	for !seen[1] {
		select {
		case rec := <-live:
			seen[rec.RowID] = true
		case <-time.After(20 * time.Second):
			t.Fatal("a write committed between Listen and the first Next was never delivered")
		}
	}
}

func TestPostgresListenNextContextCancelLeavesSessionUsable(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	live := make(chan store.ChangeRecord, 16)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", store.CursorBegin, [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	dead, killIt := context.WithCancel(ctx)
	killIt()
	if _, _, _, err := replay.Next(dead); err == nil {
		t.Fatal("Next with a cancelled context succeeded")
	}
	select {
	case cause := <-closedWith:
		t.Fatalf("a cancelled Next context ended the subscription with %v", cause)
	case <-time.After(time.Second):
	}
	drainReplay(t, ctx, replay)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "after a dead Next"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-live:
	case cause := <-closedWith:
		t.Fatalf("subscription ended with %v", cause)
	case <-time.After(20 * time.Second):
		t.Fatal("subscription stopped delivering after a cancelled Next context")
	}
}

func TestPostgresListenIdleSubscriptionOutlivesRetention(t *testing.T) {
	cfg := testConfig(t)
	retention := 2 * time.Second
	cfg.ChangeRetention = &retention
	s := openTest(t, cfg)
	ctx := t.Context()
	listenSeed(t, s, ctx)
	live := make(chan store.ChangeRecord, 16)
	closedWith := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	select {
	case cause := <-closedWith:
		t.Fatalf("idle subscription ended early with %v", cause)
	case <-time.After(3 * retention):
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "after a long idle"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-live:
		if rec.RowID != 1 {
			t.Fatalf("record: %+v", rec)
		}
	case cause := <-closedWith:
		t.Fatalf("idle subscription expired its own cursor: %v", cause)
	case <-time.After(20 * time.Second):
		t.Fatal("change after a long idle was never delivered")
	}
}

func TestPostgresListenZeroHeadReplaysNothing(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	var head int64
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		return tx.QueryRow(ctx, "SELECT next_change FROM "+s.relation("namespaces")+" WHERE name=$1", "app").Scan(&head)
	}); err != nil {
		t.Fatal(err)
	}
	if head != 0 {
		t.Fatalf("fixture precondition: next_change = %d, want 0 so the zero bound is exercised", head)
	}
	live := make(chan store.ChangeRecord, 8)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil, func(rec store.ChangeRecord) { live <- rec }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "after listen"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if replayed := drainReplay(t, ctx, replay); len(replayed) != 0 {
		t.Fatalf("replay returned %d records on a namespace whose head is zero; a post-Listen write belongs to the live phase", len(replayed))
	}
	select {
	case <-live:
	case <-time.After(20 * time.Second):
		t.Fatal("the post-Listen write never arrived on the live phase")
	}
}

func TestPostgresListenOverflowsABlockedSubscriber(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	block := make(chan struct{})
	defer close(block)
	ended := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil,
		func(store.ChangeRecord) { <-block },
		func(cause error) { ended <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	for i := 0; i < 9; i++ {
		records := make([]map[string]any, 0, store.MaxChangesPageLimit)
		for j := 0; j < store.MaxChangesPageLimit; j++ {
			records = append(records, map[string]any{"body": "flood"})
		}
		if _, err := s.Insert(context.Background(), "app", "notes", records, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case cause := <-ended:
		if !errors.Is(cause, store.ErrListenOverflow) {
			t.Fatalf("subscription ended with %v, want the overflow sentinel", cause)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a subscriber that never drained did not overflow the live buffer")
	}
}

func TestPostgresListenNeverDeliversSuccessorLifetimeRecords(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	_, inc, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan store.ChangeRecord, 64)
	ended := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, nil,
		func(r store.ChangeRecord) { got <- r }, func(c error) { ended <- c })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	if err := s.DropTable(ctx, "app", "notes", inc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, inc.NsGen); err != nil {
		t.Fatal(err)
	}
	_, successor, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "successor"}}, store.WriteOpts{}, store.Embedder{}, nil, successor); err != nil {
			t.Fatal(err)
		}
	}
	for {
		select {
		case rec := <-got:
			t.Fatalf("a subscription pinned to the predecessor received a successor-lifetime record: %+v", rec)
		case cause := <-ended:
			if !errors.Is(cause, store.ErrListenLifetimeEnded) {
				t.Fatalf("subscription ended with %v, want the lifetime sentinel", cause)
			}
			return
		case <-time.After(25 * time.Second):
			t.Fatal("a dropped table must end the subscription")
		}
	}
}

func listenAuthz(inc store.Incarnation) func(string) (*store.RowScope, store.Incarnation, bool) {
	return func(string) (*store.RowScope, store.Incarnation, bool) { return nil, inc, true }
}

func TestPostgresListenFiltersRecordsFromAnotherTableLifetime(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	_, inc, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	stale := store.Incarnation{NsGen: inc.NsGen, Table: inc.Table, DropGen: inc.DropGen + 1}
	got := make(chan store.ChangeRecord, 8)
	ended := make(chan error, 1)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, listenAuthz(stale),
		func(r store.ChangeRecord) { got <- r }, func(c error) { ended <- c })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "other lifetime"}}, store.WriteOpts{}, store.Embedder{}, nil, inc); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-got:
		t.Fatalf("a record whose lifetime differs from the authorized incarnation was delivered: %+v", rec)
	case cause := <-ended:
		t.Fatalf("a lifetime mismatch must filter the record, not end the stream: %v", cause)
	case <-time.After(3 * time.Second):
	}
}

func TestPostgresListenDeliversRecordsMatchingTheAuthorizedLifetime(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	_, inc, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan store.ChangeRecord, 8)
	replay, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, listenAuthz(inc),
		func(r store.ChangeRecord) { got <- r }, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "same lifetime"}}, store.WriteOpts{}, store.Embedder{}, nil, inc); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("a record matching the authorized incarnation was never delivered")
	}
}

func TestPostgresListenNamespaceFeedIgnoresTableLifetimeMismatch(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	listenSeed(t, s, ctx)
	_, inc, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	stale := store.Incarnation{NsGen: inc.NsGen, Table: inc.Table, DropGen: inc.DropGen + 1}
	got := make(chan store.ChangeRecord, 8)
	replay, cancel, err := s.Listen(ctx, "app", "", "", [16]byte{}, listenAuthz(stale),
		func(r store.ChangeRecord) { got <- r }, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drainReplay(t, ctx, replay)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "namespace wide"}}, store.WriteOpts{}, store.Embedder{}, nil, inc); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("a namespace-wide feed must not apply the per-record table-lifetime comparison")
	}
}

func TestListenCauseMapsAVanishedNamespace(t *testing.T) {
	fk := &pgconn.PgError{Code: "23503", Message: "insert or update on table violates foreign key constraint"}
	if got := listenCause(fk); !errors.Is(got, store.ErrListenLifetimeEnded) {
		t.Fatalf("a foreign key violation against the namespace row = %v, want the lifetime sentinel", got)
	}
	other := &pgconn.PgError{Code: "42703", Message: "column does not exist"}
	if got := listenCause(other); errors.Is(got, store.ErrListenLifetimeEnded) {
		t.Fatalf("an unrelated PostgreSQL error must not be reported as a lifetime end, got %v", got)
	}
}

func seedUnlabelledBacklog(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}},
		store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "from alice"}},
		store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("changes")+" SET owner=NULL"); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresScopedListenRefusesUnlabelledBacklog(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	seedUnlabelledBacklog(t, s, ctx)

	_, cancel, err := s.Listen(ctx, "app", "notes", store.CursorBegin, [16]byte{},
		func(string) (*store.RowScope, store.Incarnation, bool) {
			return &store.RowScope{Owner: "alice"}, store.Incarnation{}, true
		}, func(store.ChangeRecord) {}, nil)
	if cancel != nil {
		cancel()
	}
	if !errors.Is(err, store.ErrScopedFeedPredatesLabels) {
		t.Fatalf("a scoped replay across records written before labelling must refuse rather than skip them in silence: %v", err)
	}
}

func TestPostgresScopedCatchUpRefusesUnlabelledBacklog(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	seedUnlabelledBacklog(t, s, ctx)

	_, _, err := s.ChangesSince(ctx, "app", "notes", store.CursorBegin, [16]byte{},
		&store.RowScope{Owner: "alice"}, store.Incarnation{}, store.Page{Limit: 10})
	if !errors.Is(err, store.ErrScopedFeedPredatesLabels) {
		t.Fatalf("changes_since owes the same refusal as the live feed: %v", err)
	}

	if _, _, err := s.ChangesSince(ctx, "app", "notes", "", [16]byte{},
		&store.RowScope{Owner: "alice"}, store.Incarnation{}, store.Page{Limit: 10}); err != nil {
		t.Fatalf("starting at the head replays nothing, so there is nothing to refuse: %v", err)
	}
}
