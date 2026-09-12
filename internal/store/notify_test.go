package store

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
)

type notifyObs struct {
	table   string
	changes ChangeRange
	sawRows []changeRow
	sawErr  error
}

func TestNotifyWakesAfterCommitOnly(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	var mu sync.Mutex
	var observed []notifyObs
	cancel := st.onCommit("test", func(table string, changes ChangeRange) {

		n, err := st.ns("test")
		if err != nil {
			mu.Lock()
			observed = append(observed, notifyObs{table: table, changes: changes, sawErr: err})
			mu.Unlock()
			return
		}
		rows, err := readChangesInRange(t, n, changes)
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, notifyObs{table: table, changes: changes, sawRows: rows, sawErr: err})
	})
	defer cancel()

	res, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "a", "score": 1},
		{"title": "b", "score": 2},
	}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 1 {
		t.Fatalf("listener woke %d times for one committed insert, want 1", len(observed))
	}
	obs := observed[0]
	if obs.sawErr != nil {
		t.Fatalf("listener read failed: %v", obs.sawErr)
	}
	if obs.table != "notes" {
		t.Errorf("notified table = %q, want notes", obs.table)
	}
	if obs.changes != res.Changes {
		t.Errorf("notified range = %+v, want the result's %+v", obs.changes, res.Changes)
	}
	if len(obs.sawRows) != int(res.Changes.Count) {
		t.Fatalf("listener saw %d committed records in its range at wake time, want %d — the wake preceded commit", len(obs.sawRows), res.Changes.Count)
	}
	for i, r := range obs.sawRows {
		if r.seq != res.Changes.First+int64(i) {
			t.Errorf("saw row %d: seq = %d, want %d (contiguous in-range)", i, r.seq, res.Changes.First+int64(i))
		}
		if r.rowID != res.Ids[i] {
			t.Errorf("saw row %d: row_id = %d, want %d (one record per inserted id)", i, r.rowID, res.Ids[i])
		}
		if r.kind != string(ChangeInsert) {
			t.Errorf("saw row %d: kind = %q, want %q", i, r.kind, ChangeInsert)
		}
	}
}

func TestNotifyZeroRangeWakesNobody(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	var wakes atomic.Int32
	cancel := st.onCommit("test", func(table string, changes ChangeRange) {
		wakes.Add(1)
		if changes.Count <= 0 {
			t.Errorf("listener woken with zero range %+v", changes)
		}
	})
	defer cancel()

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := wakes.Load(); got != 1 {
		t.Fatalf("wakes after insert = %d, want 1", got)
	}

	upd, err := st.Update(ctx, "test", "notes", "title = 'nope'", nil, map[string]any{"score": 9}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("update matching nothing: %v", err)
	}
	if upd.Updated != 0 || upd.Changes.Count != 0 {
		t.Fatalf("update matching nothing = %+v, want zero updated and zero range", upd)
	}
	if got := wakes.Load(); got != 1 {
		t.Fatalf("wakes after zero-range update = %d, want 1 (no wake)", got)
	}

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "b"}}, WriteOpts{IdempotencyKey: "k1"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert with idempotency key: %v", err)
	}
	if got := wakes.Load(); got != 2 {
		t.Fatalf("wakes after keyed insert = %d, want 2", got)
	}
	replay, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "b"}}, WriteOpts{IdempotencyKey: "k1"}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if !replay.Replayed || replay.Changes.Count != 0 {
		t.Fatalf("replay = replayed=%v changes=%+v, want replayed with zero range", replay.Replayed, replay.Changes)
	}
	if got := wakes.Load(); got != 2 {
		t.Fatalf("wakes after idempotent replay = %d, want 2 (the replay mints nothing)", got)
	}
}

func TestNotifyEveryWritePathWakes(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	var mu sync.Mutex
	var ranges []ChangeRange
	cancel := st.onCommit("test", func(table string, changes ChangeRange) {
		mu.Lock()
		defer mu.Unlock()
		if table == "notes" {
			ranges = append(ranges, changes)
		}
	})
	defer cancel()

	ins, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a"}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"}, []map[string]any{{"title": "b", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("upsert_by_key: %v", err)
	}
	upd, err := st.Update(ctx, "test", "notes", "title = 'b'", nil, map[string]any{"score": 2}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Changes.Count != 1 {
		t.Fatalf("update changes = %+v, want 1 record", upd.Changes)
	}
	del, err := st.Delete(ctx, "test", "notes", "title = 'b'", nil, DeleteOpts{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if del.Changes.Count != 1 {
		t.Fatalf("delete changes = %+v, want 1 record", del.Changes)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 4 {
		t.Fatalf("listener woke %d times across the four write paths, want 4: %+v", len(ranges), ranges)
	}
	for i, r := range ranges {
		if r.Count != 1 {
			t.Errorf("wake %d range = %+v, want a single record", i, r)
		}
		if want := (ChangeRange{First: ins.Changes.First + int64(i), Last: ins.Changes.First + int64(i), Count: 1}); r != want {
			t.Errorf("wake %d range = %+v, want %+v (consecutive one-record mints in write order)", i, r, want)
		}
	}
}

func TestNotifyListenerPanicCannotFailWrite(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	cancelPanic := st.onCommit("test", func(table string, changes ChangeRange) {
		panic("listener bug")
	})
	defer cancelPanic()
	var wakes atomic.Int32
	cancelOk := st.onCommit("test", func(table string, changes ChangeRange) {
		wakes.Add(1)
	})
	defer cancelOk()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(old) })

	res, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a"}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert with a panicking listener: %v — the write already committed; a listener bug must not fail it", err)
	}
	if len(res.Ids) != 1 || res.Changes.Count != 1 {
		t.Fatalf("insert result = ids %v changes %+v, want one row one record", res.Ids, res.Changes)
	}
	if wakes.Load() != 1 {
		t.Fatalf("listener after the panicking one woke %d times, want 1 — recovery must not skip the rest", wakes.Load())
	}
	if msg := buf.String(); !bytes.Contains([]byte(msg), []byte("listener bug")) {
		t.Errorf("panic not logged: log = %q", msg)
	}

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	var cnt int
	if err := n.ro.QueryRowContext(ctx, `SELECT count(*) FROM notes WHERE id = ?`, res.Ids[0]).Scan(&cnt); err != nil {
		t.Fatalf("count committed rows: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("committed row count = %d, want 1", cnt)
	}
}

func TestNotifyCancelStopsDelivery(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	var wakes atomic.Int32
	cancel := st.onCommit("test", func(table string, changes ChangeRange) { wakes.Add(1) })
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	cancel()
	cancel()

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "b"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert after cancel: %v", err)
	}
	if got := wakes.Load(); got != 1 {
		t.Fatalf("wakes after cancel = %d, want 1", got)
	}
}

func TestNotifyPerNamespace(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	mustNS(t, legacy(st), "other")
	if _, err := st.CreateTable(ctx, "other", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table in other: %v", err)
	}

	var otherWakes atomic.Int32
	cancelOther := st.onCommit("other", func(table string, changes ChangeRange) { otherWakes.Add(1) })
	defer cancelOther()

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert into test: %v", err)
	}
	if got := otherWakes.Load(); got != 0 {
		t.Fatalf("other namespace woke %d times for a test write, want 0", got)
	}

	if _, err := st.Insert(ctx, "other", "notes", []map[string]any{{"title": "x"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert into other: %v", err)
	}
	if got := otherWakes.Load(); got != 1 {
		t.Fatalf("other namespace woke %d times after its own write, want 1", got)
	}
}

func TestNotifyConcurrentWritesAndRegistration(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	const perWriter = 20
	var steady atomic.Int32
	cancel := st.onCommit("test", func(table string, changes ChangeRange) {
		steady.Add(1)
		if table != "notes" {
			t.Errorf("wake table = %q, want notes", table)
		}
		if changes.Count <= 0 {
			t.Errorf("wake with empty range %+v", changes)
		}
	})
	defer cancel()

	var churnWG sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		churnWG.Add(1)
		go func() {
			defer churnWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				st.onCommit("test", func(table string, changes ChangeRange) {})()
			}
		}()
	}

	var writerWG sync.WaitGroup
	errs := make(chan error, 4)
	for w := 0; w < 4; w++ {
		writerWG.Add(1)
		go func(w int) {
			defer writerWG.Done()
			var err error
			switch w {
			case 0:
				for i := 0; i < perWriter; i++ {
					if _, err = st.Insert(ctx, "test", "notes", []map[string]any{
						{"title": fmt.Sprintf("w%d-%d", w, i)},
					}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
						errs <- fmt.Errorf("insert w%d-%d: %w", w, i, err)
						return
					}
				}
			case 1:
				for i := 0; i < perWriter; i++ {
					if _, err = st.UpsertByKey(ctx, "test", "notes", []string{"title"}, []map[string]any{
						{"title": fmt.Sprintf("w%d-key", w), "score": float64(i)},
					}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
						errs <- fmt.Errorf("upsert_by_key %d: %w", i, err)
						return
					}
				}
			case 2:
				for i := 0; i < perWriter; i++ {
					if _, err = st.Insert(ctx, "test", "notes", []map[string]any{
						{"title": fmt.Sprintf("w%d-%d", w, i)},
					}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
						errs <- fmt.Errorf("insert w%d-%d: %w", w, i, err)
						return
					}
					if _, err = st.Update(ctx, "test", "notes",
						fmt.Sprintf("title = 'w%d-%d'", w, i), nil,
						map[string]any{"score": float64(i)}, Embedder{}, nil, Incarnation{}); err != nil {
						errs <- fmt.Errorf("update w%d-%d: %w", w, i, err)
						return
					}
				}
			case 3:
				for i := 0; i < perWriter; i++ {
					if _, err = st.Insert(ctx, "test", "notes", []map[string]any{
						{"title": fmt.Sprintf("w%d-%d", w, i)},
					}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
						errs <- fmt.Errorf("insert w%d-%d: %w", w, i, err)
						return
					}
					if _, err = st.Delete(ctx, "test", "notes",
						fmt.Sprintf("title = 'w%d-%d'", w, i), nil, DeleteOpts{}, nil, Incarnation{}); err != nil {
						errs <- fmt.Errorf("delete w%d-%d: %w", w, i, err)
						return
					}
				}
			}
			errs <- nil
		}(w)
	}

	writerWG.Wait()
	close(stop)
	churnWG.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed: %v", err)
		}
	}

	const wantWakes = 6 * perWriter
	if got := steady.Load(); got != wantWakes {
		t.Fatalf("steady listener woke %d times, want %d (once per non-empty write)", got, wantWakes)
	}
}

func readChangesInRange(t *testing.T, n *nsDB, changes ChangeRange) ([]changeRow, error) {
	t.Helper()
	rows, err := n.ro.QueryContext(context.Background(),
		`SELECT seq, table_name, row_id, kind, owner, nsgen, drop_gen, at FROM _dolmen_changes WHERE seq BETWEEN ? AND ? ORDER BY seq`,
		changes.First, changes.Last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []changeRow
	for rows.Next() {
		var r changeRow
		if err := rows.Scan(&r.seq, &r.table, &r.rowID, &r.kind, &r.owner, &r.nsGen, &r.dropGen, &r.at); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
