package postgres

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

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
	s.wakes().mu.Lock()
	waiters := s.wake.waiters
	s.wake.waiters = map[string]map[chan struct{}]struct{}{}
	s.wakes().mu.Unlock()
	defer func() {
		s.wakes().mu.Lock()
		s.wake.waiters = waiters
		s.wakes().mu.Unlock()
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
	_, cancel, err := s.Listen(ctx, "app", "notes", "", [16]byte{}, func(string) (*store.RowScope, store.Incarnation, bool) {
		return nil, store.Incarnation{}, admitted.Load()
	}, func(store.ChangeRecord) {}, func(cause error) { closedWith <- cause })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	admitted.Store(false)
	select {
	case cause := <-closedWith:
		if cause == nil {
			t.Fatal("revoked admission closed without a cause")
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
		if cause != nil {
			t.Fatalf("cancel reported %v", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancel never closed the subscription")
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
