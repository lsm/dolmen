package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

func TestNamespaceListCreateDrop(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	nss, err := st.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nss) != 0 {
		t.Fatalf("fresh store must list no namespaces, got %v", nss)
	}

	if err := st.CreateNamespace("alpha"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "alpha.db")); err != nil {
		t.Fatalf("create must materialize the namespace file: %v", err)
	}
	if err := st.CreateNamespace("alpha"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate create must fail with ErrInvalid, got %v", err)
	}
	if err := st.CreateNamespace("../escape"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid name must fail with ErrInvalid, got %v", err)
	}
	if err := st.DropNamespace("../escape"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("drop of invalid name must fail with ErrInvalid, got %v", err)
	}

	mustCreateNotes(t, st) // implicitly creates namespace "test" with a WAL
	nss, err = st.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nss) != 2 || nss[0] != "alpha" || nss[1] != "test" {
		t.Fatalf("expected [alpha test], got %v", nss)
	}

	if err := st.DropNamespace("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop of missing namespace must 404, got %v", err)
	}

	if err := st.DropNamespace("test"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(st.dir, "test.db"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("test.db%s must be gone after drop", suffix)
		}
	}
	if _, ok := st.nss["test"]; ok {
		t.Fatal("drop must evict the cached connections")
	}

	nss, err = st.ListNamespaces()
	if err != nil {
		t.Fatalf("list after drop: %v", err)
	}
	if len(nss) != 1 || nss[0] != "alpha" {
		t.Fatalf("expected [alpha] after drop, got %v", nss)
	}

	// The name is reusable: recreated empty, none of the old tables.
	if err := st.CreateNamespace("test"); err != nil {
		t.Fatalf("recreate dropped namespace: %v", err)
	}
	tables, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("list tables on recreated namespace: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("recreated namespace must be empty, got %v", tables)
	}
}

func TestDropNamespaceSurvivesRestartWithWAL(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "wal row"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err = Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.DropNamespace("test"); err != nil {
		t.Fatalf("drop after restart: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, "test.db"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("test.db%s must be gone after drop with a WAL present", suffix)
		}
	}
	nss, err := st.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nss) != 0 {
		t.Fatalf("expected no namespaces after drop, got %v", nss)
	}
}

func TestDropNamespaceWithConcurrentWriters(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Errors are expected once the drop lands (the table, then the
				// namespace, vanish underneath the writers); what must not
				// happen is a deadlock, a panic, or the drop failing.
				_, _ = st.Insert(ctx, "test", "notes", []map[string]any{{"title": "racing"}}, testEmbed)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond) // let the writers engage
	if err := st.DropNamespace("test"); err != nil {
		t.Fatalf("drop under concurrency must succeed, got %v", err)
	}
	close(stop)
	wg.Wait()

	// The namespace is either gone or — when a racing writer recreated the
	// name after the drop — exists again but fresh: the old table must not
	// survive in either case.
	tables, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("post-drop state must be queryable: %v", err)
	}
	for _, tb := range tables {
		if tb == "notes" {
			t.Fatal("dropped table must not survive a concurrent recreate")
		}
	}
}

func TestDropTable(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	if _, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "extra", Type: schema.String}},
	}, testEmbed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, _, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "once"}}, testEmbed, "op-1"); err != nil {
		t.Fatalf("idempotent insert: %v", err)
	}

	if err := st.DropTable(ctx, "test", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop of missing table must 404, got %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop: %v", err)
	}

	tables, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("expected no tables after drop, got %v", tables)
	}

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	count := func(sql string) int64 {
		t.Helper()
		var c int64
		if err := n.ro.QueryRowContext(ctx, sql).Scan(&c); err != nil {
			t.Fatalf("count %q: %v", sql, err)
		}
		return c
	}
	if c := count(`SELECT count(*) FROM sqlite_master WHERE name IN ('notes', 'notes__fts')`); c != 0 {
		t.Fatalf("table and FTS shadow must be gone from sqlite_master, got %d rows", c)
	}
	if c := count(`SELECT count(*) FROM _dolmen_tables WHERE name = 'notes'`); c != 0 {
		t.Fatal("registry row must be gone")
	}
	if c := count(`SELECT count(*) FROM _dolmen_migrations WHERE table_name = 'notes'`); c != 0 {
		t.Fatal("migration history must be gone")
	}
	if c := count(`SELECT count(*) FROM _dolmen_idempotency WHERE table_name = 'notes'`); c != 0 {
		t.Fatal("idempotency keys must be gone")
	}

	// Recreating the name starts fresh: version 1, and the dropped table's
	// idempotency key does not replay the old ids.
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields()); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "notes")
	if err != nil {
		t.Fatalf("describe recreated table: %v", err)
	}
	if sc.Version != 1 {
		t.Fatalf("recreated table must be version 1, got %d", sc.Version)
	}
	if sc.Field("extra") != nil {
		t.Fatal("recreated table must not inherit dropped-table fields")
	}
	_, replayed, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "fresh"}}, testEmbed, "op-1")
	if err != nil {
		t.Fatalf("insert with the old key on the recreated table: %v", err)
	}
	if replayed {
		t.Fatal("a recreated table must not replay the dropped table's idempotency keys")
	}
}

func TestDropTableRemovesSearch(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "findme"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := st.SearchFulltext(ctx, "test", "notes", "findme", 10, false); err != nil {
		t.Fatalf("search before drop: %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, _, err := st.SearchFulltext(ctx, "test", "notes", "findme", 10, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("search on dropped table must 404, got %v", err)
	}
}
