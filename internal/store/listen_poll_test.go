package store

import (
	"context"
	"testing"
	"time"
)

func TestListenPollDeliversForeignStoreCommits(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mustNS(t, legacy(st), "test")
	if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	insertNotes(t, st, 1)

	foreign, err := Open(dir)
	if err != nil {
		t.Fatalf("open foreign store: %v", err)
	}
	t.Cleanup(func() { foreign.Close() })

	got := make(chan ChangeRecord, 2)
	replay, cancel := listenOn(t, st, "", "", func(r ChangeRecord) { got <- r }, nil)
	defer cancel()
	drainReplay(t, replay)

	res, err := foreign.Insert(context.Background(), "test", "notes", []map[string]any{{"title": "x", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("foreign insert: %v", err)
	}
	if res.Changes.Count != 1 {
		t.Fatalf("foreign insert minted %d changes, want 1", res.Changes.Count)
	}

	select {
	case r := <-got:
		if r.RowID != int64(2) {
			t.Fatalf("polled record arrived at row %d, want 2", r.RowID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a foreign store's commit was never delivered — the poll fallback did not re-read the log")
	}
}

func TestListenPollPumpStopsAtEnd(t *testing.T) {
	st := openChangeStore(t)

	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, nil)
	sess := trackedSnapshot(st, "test")[0]
	drainReplay(t, replay)

	done := make(chan struct{})
	go func() {
		cancel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel never returned — teardown sat out the poll ticker")
	}
	select {
	case <-sess.stop:
	default:
		t.Fatal("end left the poll pump's stop channel open")
	}
}

func trackedSnapshot(st *Store, ns string) []*listenSession {
	st.notifyMu.Lock()
	defer st.notifyMu.Unlock()
	return append([]*listenSession(nil), st.listenSessions[ns]...)
}
