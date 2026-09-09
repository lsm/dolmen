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

// Slice 4d: the post-commit notification registry (§9.3). Listeners wake
// after commit only — a listener that reads the change log from inside its
// callback, through the namespace's separate read pool, must find the minted
// records already visible, which is only possible once tx.Commit() returned.
// A panicking listener can never fail the write it observes (recovered,
// logged, remaining listeners still run); a write that minted no records
// wakes nobody; and the registry is safe under concurrent writes and
// registration churn — the race-detector contract for this slice.

// notifyObs is what one listener wake records: the notified table+range, and
// what a fresh read of the log saw at wake time — the "after commit" proof.
type notifyObs struct {
	table   string
	changes ChangeRange
	sawRows []changeRow
	sawErr  error
}

func TestNotifyWakesAfterCommitOnly(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	var mu sync.Mutex // guards observed: the listener runs on the write goroutine
	var observed []notifyObs
	cancel := st.onCommit("test", func(table string, changes ChangeRange) {
		// Read the log through the read pool — a connection the write
		// transaction never held. Had this wake fired before commit, the
		// range's records would be invisible here (SQLite never exposes
		// another connection's in-flight transaction); seeing them proves
		// commit preceded the wake.
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

	// The wake is synchronous with the write: it has already run.
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

// TestNotifyZeroRangeWakesNobody: a write that minted no records never moved
// the log head, so waking a waiter would only send it re-scanning into
// nothing. Pinned on both zero-range shapes: a filter that matches nothing,
// and an idempotent replay (the original insert already woke for the same
// records).
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

	// Update matching nothing: no records minted, no wake.
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

	// Idempotent replay: the original insert's wake stands, the replay adds none.
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

// TestNotifyEveryWritePathWakes: all four write paths — insert, upsert_by_key,
// update/upsert, delete — notify with the range their transaction minted,
// once per commit, in write order.
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

// TestNotifyListenerPanicCannotFailWrite: the panic is recovered and logged,
// the write — already committed — returns success with its row in place, and
// listeners registered after the panicking one still run.
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

	// Capture the default logger's output to pin the "logged" half of the
	// acceptance.
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

	// The row is really there: the write committed despite the panic.
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

// TestNotifyCancelStopsDelivery: after cancel returns, no later dispatch
// selects the listener — the registration handle 6b's session teardown holds.
func TestNotifyCancelStopsDelivery(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	var wakes atomic.Int32
	cancel := st.onCommit("test", func(table string, changes ChangeRange) { wakes.Add(1) })
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	cancel()
	cancel() // idempotent

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "b"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert after cancel: %v", err)
	}
	if got := wakes.Load(); got != 1 {
		t.Fatalf("wakes after cancel = %d, want 1", got)
	}
}

// TestNotifyPerNamespace: the registry is per-namespace — a write to one
// namespace never wakes another namespace's waiters.
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

// TestNotifyConcurrentWritesAndRegistration is the race-slice test: all four
// write paths committing concurrently while listener registration churns on
// the same namespace. Every write succeeds; the steady listener — registered
// once, never cancelled — wakes exactly once per non-empty write.
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

	// Registration churn: listeners come and go while writes land — the
	// registry mutex and the dead-flag are what keep this race-free. Stops
	// only after the writers finish, so no dispatch outlives the test.
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

	// Writers on disjoint title prefixes, so concurrent filters never touch
	// each other's rows. Every call mints at least one record.
	var writerWG sync.WaitGroup
	errs := make(chan error, 4)
	for w := 0; w < 4; w++ {
		writerWG.Add(1)
		go func(w int) {
			defer writerWG.Done()
			var err error
			switch w {
			case 0: // insert path
				for i := 0; i < perWriter; i++ {
					if _, err = st.Insert(ctx, "test", "notes", []map[string]any{
						{"title": fmt.Sprintf("w%d-%d", w, i)},
					}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
						errs <- fmt.Errorf("insert w%d-%d: %w", w, i, err)
						return
					}
				}
			case 1: // upsert_by_key path: the first call inserts, the rest update
				for i := 0; i < perWriter; i++ {
					if _, err = st.UpsertByKey(ctx, "test", "notes", []string{"title"}, []map[string]any{
						{"title": fmt.Sprintf("w%d-key", w), "score": float64(i)},
					}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
						errs <- fmt.Errorf("upsert_by_key %d: %w", i, err)
						return
					}
				}
			case 2: // update path
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
			case 3: // delete path
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

	// w0/w1 commit perWriter writes each; w2/w3 commit two non-empty writes
	// per iteration (insert + update, insert + delete) — 6 × perWriter.
	const wantWakes = 6 * perWriter
	if got := steady.Load(); got != wantWakes {
		t.Fatalf("steady listener woke %d times, want %d (once per non-empty write)", got, wantWakes)
	}
}

// readChangesInRange reads the committed change records with seq in
// [changes.First, changes.Last] — the listener-side "what is durable right
// now" view.
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
