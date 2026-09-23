package store

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

func openCapped(t *testing.T, max int) legacyStore {
	t.Helper()
	st, err := Open(t.TempDir(), WithMaxOpenNamespaces(max))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return legacy(st)
}

func (l legacyStore) handleCounts() (open int, pins int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, n := range l.nss {
		pins += n.pins.Load()
	}
	return len(l.nss), pins
}

func (l legacyStore) handle(ns string) *nsDB {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nss[ns]
}

func seedNotes(t *testing.T, st legacyStore, names ...string) {
	t.Helper()
	ctx := context.Background()
	for _, ns := range names {
		mustNS(t, st, ns)
		if _, err := st.CreateTable(ctx, ns, "notes", noteFields()); err != nil {
			t.Fatalf("create table in %s: %v", ns, err)
		}
		if _, err := st.Insert(ctx, ns, "notes", []map[string]any{{"title": ns, "body": "seeded " + ns, "emb": []float32{1, 0, 0, 0}}}, testEmbed); err != nil {
			t.Fatalf("insert into %s: %v", ns, err)
		}
	}
}

func TestOpenRefusesACapBelowOne(t *testing.T) {
	if _, err := Open(t.TempDir(), WithMaxOpenNamespaces(0)); err == nil {
		t.Fatal("a store that may hold no namespace open cannot serve one; Open must refuse it")
	}
}

func TestIdleNamespacesCloseOncePastTheCap(t *testing.T) {
	st := openCapped(t, 3)
	var names []string
	for i := 0; i < 10; i++ {
		names = append(names, fmt.Sprintf("ns%d", i))
		seedNotes(t, st, names[i])
		if open, _ := st.handleCounts(); open > 3 {
			t.Fatalf("after %d namespaces, %d are open; the cap is 3", i+1, open)
		}
	}
	for _, ns := range names {
		rows, _, err := st.Query(context.Background(), ns, "SELECT title FROM notes", nil, 0, 10)
		if err != nil || len(rows) != 1 || rows[0]["title"] != ns {
			t.Fatalf("%s, reopened after it was closed: %v %v", ns, rows, err)
		}
	}
	if open, pins := st.handleCounts(); open > 3 || pins != 0 {
		t.Fatalf("%d open and %d pins; want at most 3 open and no pin left once every operation returned", open, pins)
	}
}

func TestTheLeastRecentlyUsedNamespaceClosesFirst(t *testing.T) {
	st := openCapped(t, 2)
	seedNotes(t, st, "a", "b")
	if _, _, err := st.Query(context.Background(), "a", "SELECT 1", nil, 0, 1); err != nil {
		t.Fatal(err)
	}
	seedNotes(t, st, "c")
	if st.handle("a") == nil || st.handle("b") != nil {
		t.Fatal("b was used least recently, so b closes and a stays open")
	}
}

func TestReadPoolsAreBounded(t *testing.T) {
	st := openCapped(t, 2)
	seedNotes(t, st, "a")
	n := st.handle("a")
	if got := n.ro.Stats().MaxOpenConnections; got != readConnsPerNS {
		t.Fatalf("read pool allows %d connections, want %d", got, readConnsPerNS)
	}
	if got := n.rw.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("write pool allows %d connections, want the single writer", got)
	}
}

func TestASubscribedNamespaceStaysOpenPastTheCap(t *testing.T) {
	st := openCapped(t, 2)
	seedNotes(t, st, "live")
	ctx := context.Background()
	got := make(chan ChangeRecord, 16)
	replay, stop, err := st.Store.Listen(ctx, "live", "notes", "", [16]byte{}, nil, func(r ChangeRecord) { got <- r }, nil)
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, _, done, err := replay.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	held := st.handle("live")
	seedNotes(t, st, "a", "b", "c", "d")
	if st.handle("live") != held {
		t.Fatal("a namespace with a live subscription was closed to make room for others")
	}
	if _, err := st.Insert(ctx, "live", "notes", []map[string]any{{"title": "after the churn"}}, testEmbed); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the subscription stopped delivering once other namespaces churned")
	}
	stop()
	deadline := time.Now().Add(10 * time.Second)
	for held.pins.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the ended subscription still pins its namespace (%d pins)", held.pins.Load())
		}
		time.Sleep(time.Millisecond)
	}
	seedNotes(t, st, "e", "f")
	if st.handle("live") != nil {
		t.Fatal("once its subscription ended, the idle namespace must be closable again")
	}
}

func TestNamespaceChurnUnderConcurrentUse(t *testing.T) {
	st := openCapped(t, 3)
	var names []string
	for i := 0; i < 8; i++ {
		names = append(names, fmt.Sprintf("churn%d", i))
	}
	seedNotes(t, st, names...)
	const workers = 6
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			ctx := context.Background()
			for i := 0; i < 150; i++ {
				ns := names[rng.Intn(len(names))]
				var err error
				switch i % 5 {
				case 0:
					_, err = st.Insert(ctx, ns, "notes", []map[string]any{{"title": "churn"}}, testEmbed)
				case 1:
					_, _, err = st.Query(ctx, ns, "SELECT count(*) AS n FROM notes", nil, 0, 1)
				case 2:
					_, _, err = st.SearchFulltext(ctx, ns, "notes", "seeded", 0, 5, false, "", nil)
				case 3:
					_, _, err = st.DescribeTable(ctx, ns, "notes")
				case 4:
					_, err = st.Update(ctx, ns, "notes", "title = ?", []any{"churn"}, map[string]any{"score": i}, testEmbed)
				}
				if err != nil {
					errs <- fmt.Errorf("worker %d, operation %d on %s: %w", w, i%5, ns, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if open, pins := st.handleCounts(); open > 3+workers || pins != 0 {
		t.Fatalf("%d open and %d pins; the ceiling is the cap plus what is in use (%d), and no pin may outlive its operation", open, pins, 3+workers)
	}
}

func TestEveryOperationReleasesItsNamespace(t *testing.T) {
	st := openCapped(t, 2)
	raw := st.Store
	seedNotes(t, st, "leak")
	ctx := context.Background()
	_, inc, err := raw.TableState(ctx, "leak", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := raw.NamespaceState(ctx, "leak", nil)
	if err != nil {
		t.Fatal(err)
	}
	addField := []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "priority", Type: schema.Number}}}
	ops := []struct {
		name    string
		failing bool
		run     func() error
	}{
		{"insert", false, func() error {
			_, err := raw.Insert(ctx, "leak", "notes", []map[string]any{{"title": "x"}}, WriteOpts{IdempotencyKey: "k"}, testEmbed, nil, inc)
			return err
		}},
		{"upsert_by_key", false, func() error {
			_, err := raw.UpsertByKey(ctx, "leak", "notes", []string{"title"}, []map[string]any{{"title": "x", "score": 2}}, WriteOpts{}, testEmbed, nil, inc)
			return err
		}},
		{"upsert", false, func() error {
			_, err := raw.Upsert(ctx, "leak", "notes", "title = ?", []any{"y"}, map[string]any{"title": "y"}, WriteOpts{}, testEmbed, nil, inc)
			return err
		}},
		{"update", false, func() error {
			_, err := raw.Update(ctx, "leak", "notes", "title = ?", []any{"y"}, map[string]any{"score": 3}, testEmbed, nil, inc)
			return err
		}},
		{"query", false, func() error {
			_, err := raw.Query(ctx, "leak", "SELECT title FROM notes", nil, gen, Page{})
			return err
		}},
		{"query that fails", true, func() error {
			_, err := raw.Query(ctx, "leak", "SELECT nope FROM notes", nil, gen, Page{})
			return err
		}},
		{"search_fulltext", false, func() error {
			_, err := raw.SearchFulltext(ctx, "leak", "notes", "seeded", "", nil, false, nil, inc, Page{})
			return err
		}},
		{"search_vector", false, func() error {
			_, err := raw.SearchVector(ctx, "leak", "notes", VectorQuery{Column: "emb", Vec: []float32{1, 0, 0, 0}}, false, nil, inc, Page{})
			return err
		}},
		{"get_rows", false, func() error {
			_, err := raw.GetRows(ctx, "leak", "notes", []int64{1}, nil, inc)
			return err
		}},
		{"changes_since", false, func() error {
			_, _, err := raw.ChangesSince(ctx, "leak", "notes", CursorBegin, gen, nil, inc, Page{})
			return err
		}},
		{"describe_table", false, func() error {
			_, _, err := raw.DescribeTable(ctx, "leak", "notes", nil, inc)
			return err
		}},
		{"list_tables", false, func() error {
			_, err := raw.ListTables(ctx, "leak", nil)
			return err
		}},
		{"delete dry run", false, func() error {
			_, err := raw.Delete(ctx, "leak", "notes", "title = ?", []any{"y"}, DeleteOptions{DryRun: true}, nil, inc)
			return err
		}},
		{"delete", false, func() error {
			_, err := raw.Delete(ctx, "leak", "notes", "title = ?", []any{"y"}, DeleteOptions{}, nil, inc)
			return err
		}},
		{"plan_migration", false, func() error {
			_, err := raw.PlanMigration(ctx, "leak", "notes", addField, testEmbed, inc, nil, inc)
			return err
		}},
		{"migrate", false, func() error {
			_, err := raw.Migrate(ctx, "leak", "notes", addField, testEmbed, inc)
			return err
		}},
		{"list_migrations", false, func() error {
			_, err := raw.ListMigrations(ctx, "leak", "notes", Incarnation{})
			return err
		}},
		{"insert against a stale incarnation", true, func() error {
			_, err := raw.Insert(ctx, "leak", "notes", []map[string]any{{"title": "z"}}, WriteOpts{}, testEmbed, nil, inc)
			return err
		}},
		{"subscribe", false, func() error {
			_, stop, err := raw.Listen(ctx, "leak", "notes", "", gen, nil, func(ChangeRecord) {}, nil)
			if err != nil {
				return err
			}
			stop()
			return nil
		}},
		{"drop_table", false, func() error {
			return raw.DropTable(ctx, "leak", "notes", Incarnation{})
		}},
	}
	n := st.handle("leak")
	for _, op := range ops {
		err := op.run()
		if op.failing != (err != nil) {
			t.Fatalf("%s: err = %v, want failure %v", op.name, err, op.failing)
		}
		settle := time.Now()
		if op.name == "subscribe" {
			settle = settle.Add(10 * time.Second)
		}
		for n.pins.Load() != 0 {
			if time.Now().After(settle) {
				t.Fatalf("%s left %d pins on its namespace, so the namespace could never be closed", op.name, n.pins.Load())
			}
			time.Sleep(time.Millisecond)
		}
	}
}
