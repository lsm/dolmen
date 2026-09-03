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
	// survive in either case, and the post-drop namespace must be fully
	// usable. A straggler connection from the evicted pools closing after the
	// namespace's recreation deletes its WAL sidecars by path, leaving the
	// new pools poisoned (read-only opens then fail with disk I/O errors) —
	// evict drains precisely to prevent that.
	tables, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("post-drop state must be queryable: %v", err)
	}
	for _, tb := range tables {
		if tb == "notes" {
			t.Fatal("dropped table must not survive a concurrent recreate")
		}
	}
	if _, err := st.CreateTable(ctx, "test", "fresh", []schema.Field{
		{Name: "x", Type: schema.String},
	}); err != nil {
		t.Fatalf("post-drop namespace must accept writes: %v", err)
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
	if c := count(`SELECT count(*) FROM _dolmen_drop_gen WHERE table_name = 'notes' AND gen = 1`); c != 1 {
		t.Fatal("drop generation must be persisted at 1")
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

// pausingEmbedder returns an embedder whose first call blocks until released
// (letting a test drop + recreate the table mid-embed), then behaves like
// fakeEmbed for this and all later calls — as the insert retry loop requires.
func pausingEmbedder() (Embedder, func(), func()) {
	paused := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	return Embedder{
		Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
			once.Do(func() { close(paused); <-release })
			return fakeEmbed(ctx, texts)
		},
		Identity: "fake-space",
	}, func() { <-paused }, func() { close(release) }
}

// recreatedFields is noteFields with score flipped from number to boolean: a
// recreate under the same name and version that a stale write must not accept
// (0.9 coerces cleanly against the old number field, not the new boolean one).
func recreatedFields() []schema.Field {
	f := noteFields()
	for i := range f {
		if f[i].Name == "score" {
			f[i].Type = schema.Boolean
		}
	}
	return f
}

func TestDropTableDuringInsertEmbedPause(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	emb, waitPaused, release := pausingEmbedder()
	type outcome struct {
		ids []int64
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ids, err := st.Insert(ctx, "test", "notes", []map[string]any{
			{"title": "stale", "body": "validated against the dead schema", "score": 0.9},
		}, emb)
		done <- outcome{ids, err}
	}()

	// The insert has read the schema and is mid-embed: drop and recreate the
	// table under the same name, same version, different field types.
	waitPaused()
	if err := st.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", recreatedFields()); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	release()

	// The stale attempt must be discarded, not committed: the retry
	// re-validates against the recreated table and rejects the record.
	out := <-done
	if out.err == nil {
		t.Fatalf("stale insert must not commit into the recreated table, got ids %v", out.ids)
	}
	if !errors.Is(out.err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid from re-validation against the recreated table, got %v", out.err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatalf("recreated table must stay empty, got %v", rows)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "notes")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if f := sc.Field("score"); f == nil || f.Type != schema.Boolean {
		t.Fatalf("recreated table must keep its own schema, got %v", sc.Fields)
	}
}

func TestDropTableDuringUpsertByKeyEmbedPause(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	emb, waitPaused, release := pausingEmbedder()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"}, []map[string]any{
			{"title": "stale", "body": "validated against the dead schema", "score": 0.9},
		}, emb)
		done <- err
	}()

	waitPaused()
	if err := st.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", recreatedFields()); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	release()

	if err := <-done; err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale upsert must re-validate and fail against the recreated table, got %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatalf("recreated table must stay empty, got %v", rows)
	}
}

// The drop generation is persisted, so the guard also holds when a second
// Store instance (or a second server process) sharing the data directory
// performs the drop + recreate while this instance's write is mid-embedding.
func TestDropTableBySecondStoreInstanceDuringEmbedPause(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	stA, err := Open(dir)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	t.Cleanup(func() { stA.Close() })
	stB, err := Open(dir)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	t.Cleanup(func() { stB.Close() })
	if _, err := stA.CreateTable(ctx, "test", "notes", noteFields()); err != nil {
		t.Fatalf("create: %v", err)
	}

	emb, waitPaused, release := pausingEmbedder()
	done := make(chan error, 1)
	go func() {
		_, err := stA.Insert(ctx, "test", "notes", []map[string]any{
			{"title": "stale", "body": "validated against the dead schema", "score": 0.9},
		}, emb)
		done <- err
	}()

	waitPaused()
	if err := stB.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop via B: %v", err)
	}
	if _, err := stB.CreateTable(ctx, "test", "notes", recreatedFields()); err != nil {
		t.Fatalf("recreate via B: %v", err)
	}
	release()

	if err := <-done; err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale insert on A must re-validate and fail against B's recreated table, got %v", err)
	}
	rows, _, err := stB.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("count via B: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatalf("recreated table must stay empty, got %v", rows)
	}
}

func TestCreateNamespaceConcurrentReservation(t *testing.T) {
	st := openStore(t)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.CreateNamespace("race")
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrInvalid):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("exactly one creator must win the reservation, got %d", won)
	}
}

func TestNamespaceFileRemovedOutOfBand(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st) // namespace "test" is open and cached

	// Simulate an operator deleting the file under a live server.
	if err := os.Remove(filepath.Join(st.dir, "test.db")); err != nil {
		t.Fatalf("out-of-band remove: %v", err)
	}
	if err := st.DropNamespace("test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop of the removed namespace must 404, got %v", err)
	}
	if _, ok := st.nss["test"]; ok {
		t.Fatal("stale cache entry must be evicted (pools closed, not orphaned)")
	}

	// The name is creatable again and starts fresh.
	if err := st.CreateNamespace("test"); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	tables, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("recreated namespace must be empty, got %v", tables)
	}
}
