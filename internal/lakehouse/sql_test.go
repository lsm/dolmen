package lakehouse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

func sidecarConfig(t *testing.T) SQLEngine {
	t.Helper()
	bin := os.Getenv("DOLMEN_TEST_DUCKDB_SIDECAR")
	if bin == "" {
		if os.Getenv("DOLMEN_TEST_DUCKDB_REQUIRED") == "1" {
			t.Fatal("DOLMEN_TEST_DUCKDB_SIDECAR is required in this job")
		}
		t.Skip("DOLMEN_TEST_DUCKDB_SIDECAR not set; the lakehouse SQL tests need a built dolmen-duckdb")
	}
	return SQLEngine{Binary: bin, ExtensionDir: os.Getenv("DOLMEN_TEST_DUCKDB_EXTENSIONS"), Memory: 256 << 20}
}

func openSQLStore(t *testing.T, dir string, cfg SQLEngine) *Store {
	t.Helper()
	key, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, WithSQLEngine(cfg), WithSecretKeyring(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustQuery(t *testing.T, s *Store, ns, sql string, args ...any) store.QueryResult {
	t.Helper()
	res, err := s.Query(t.Context(), ns, sql, args, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return res
}

func TestQueryReadsCommittedRowsThroughTheSidecar(t *testing.T) {
	cfg := sidecarConfig(t)
	s := openSQLStore(t, t.TempDir(), cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "title", Type: schema.String}, {Name: "score", Type: schema.Number}, {Name: "ok", Type: schema.Boolean}, {Name: "token", Type: schema.Secret}}
	if _, err := s.CreateTable(ctx, "ns", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if res := mustQuery(t, s, "ns", "SELECT count(*) AS n FROM notes"); res.Rows[0]["n"] != int64(0) {
		t.Fatalf("an empty table counts %v", res.Rows)
	}
	if _, err := s.Insert(ctx, "ns", "notes", []map[string]any{{"title": "a", "score": 1.5, "ok": true, "token": "PLAINTEXT-1"}, {"title": "b", "score": 7}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	res := mustQuery(t, s, "ns", "SELECT id, title, score, ok, token FROM notes ORDER BY id")
	if len(res.Rows) != 2 {
		t.Fatalf("rows %v", res.Rows)
	}
	first := res.Rows[0]
	if first["id"] != int64(1) || first["title"] != "a" || first["score"] != 1.5 || first["ok"] != true || first["token"] != secret.Mask {
		t.Fatalf("first row %v", first)
	}
	if res.Rows[1]["score"] != 7.0 || res.Rows[1]["token"] != nil {
		t.Fatalf("second row %v", res.Rows[1])
	}
	if _, err := s.Insert(ctx, "ns", "notes", []map[string]any{{"title": "c"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if res := mustQuery(t, s, "ns", "SELECT count(*) AS n FROM notes WHERE title <> ?", "a"); res.Rows[0]["n"] != int64(2) {
		t.Fatalf("an acknowledged insert must be visible to the next query: %v", res.Rows)
	}
	if res := mustQuery(t, s, "ns", "SELECT length(token) AS n FROM notes WHERE id = 1"); res.Rows[0]["n"] != int64(len([]rune(secret.Mask))) {
		t.Fatalf("a secret must be measurable only as its mask: %v", res.Rows)
	}
	page, err := s.Query(ctx, "ns", "SELECT id FROM notes ORDER BY id", nil, [16]byte{}, store.Page{Offset: 1, Limit: 1})
	if err != nil || len(page.Rows) != 1 || page.Rows[0]["id"] != int64(2) || !page.Truncated {
		t.Fatalf("offset 1 limit 1 = %+v %v", page, err)
	}
}

func TestQueryIsConfinedToItsNamespace(t *testing.T) {
	cfg := sidecarConfig(t)
	dir := t.TempDir()
	s := openSQLStore(t, dir, cfg)
	ctx := t.Context()
	for _, ns := range []string{"mine", "other"} {
		if err := s.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateTable(ctx, ns, "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Insert(ctx, ns, "t", []map[string]any{{"v": "secret-of-" + ns}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	otherData := filepath.ToSlash(filepath.Join(dir, "other.lakehouse", "data"))
	catalog := filepath.ToSlash(filepath.Join(dir, "mine.lakehouse", "catalog.db"))
	for _, sql := range []string{
		"SELECT * FROM read_parquet('" + otherData + "/**/*.parquet')",
		"SELECT * FROM glob('" + otherData + "/**')",
		"SELECT * FROM read_text('" + catalog + "')",
		"SELECT * FROM read_text('/etc/passwd')",
		"SELECT getenv('HOME') AS h",
		"SELECT * FROM read_csv('https://example.com/x.csv')",
	} {
		res, err := s.Query(ctx, "mine", sql, nil, [16]byte{}, store.Page{})
		if err == nil {
			t.Fatalf("%s escaped the namespace: %v", sql, res.Rows)
		}
		if strings.Contains(err.Error(), "secret-of-other") {
			t.Fatalf("%s leaked another namespace in its error: %v", sql, err)
		}
	}
	if res := mustQuery(t, s, "mine", "SELECT v FROM t"); len(res.Rows) != 1 || res.Rows[0]["v"] != "secret-of-mine" {
		t.Fatalf("the namespace's own rows: %v", res.Rows)
	}
}

func TestQueryRefusesWritesAndSeveralStatements(t *testing.T) {
	cfg := sidecarConfig(t)
	s := openSQLStore(t, t.TempDir(), cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"SELECT 1; SELECT 2", "DELETE FROM t", "WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x"} {
		if _, err := s.Query(ctx, "ns", sql, nil, [16]byte{}, store.Page{}); err == nil {
			t.Fatalf("%s was accepted", sql)
		}
	}
	_, err := s.Query(ctx, "ns", "SELECT nope FROM t", nil, [16]byte{}, store.Page{})
	if ops.Classify(err) != derr.Query {
		t.Fatalf("SQL the engine rejects is a query_error: %v", err)
	}
}

func TestACancelledQueryLeavesTheSidecarUsable(t *testing.T) {
	cfg := sidecarConfig(t)
	s := openSQLStore(t, t.TempDir(), cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Query(short, "ns", "SELECT count(*) FROM range(100000000000) a", nil, [16]byte{}, store.Page{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a query past its deadline must answer with the deadline: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the cancel took %v", took)
	}
	if res := mustQuery(t, s, "ns", "SELECT 41 + 1 AS n"); res.Rows[0]["n"] != int64(42) {
		t.Fatalf("the sidecar must serve the next query: %v", res.Rows)
	}
}

func TestQueryWithoutASidecarTeaches(t *testing.T) {
	s := openSQLStore(t, t.TempDir(), SQLEngine{})
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Query(ctx, "ns", "SELECT 1", nil, [16]byte{}, store.Page{})
	if !errors.Is(err, ErrSQLEngineUnavailable) || !strings.Contains(err.Error(), "dolmen-duckdb") {
		t.Fatalf("a missing sidecar must teach: %v", err)
	}
}

func TestRawDataFilesHoldNoSecretForCallerSQL(t *testing.T) {
	cfg := sidecarConfig(t)
	dir := t.TempDir()
	s := openSQLStore(t, dir, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "creds", []schema.Field{{Name: "token", Type: schema.Secret}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "creds", []map[string]any{{"token": "PLAINTEXT-x"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	data := filepath.ToSlash(filepath.Join(s.dir, "ns.lakehouse", "data"))
	res := mustQuery(t, s, "ns", "SELECT token FROM read_parquet('"+data+"/**/*.parquet')")
	if len(res.Rows) != 1 {
		t.Fatalf("rows %v", res.Rows)
	}
	if raw, _ := res.Rows[0]["token"].([]byte); len(raw) > 1 {
		t.Fatalf("a raw read of the data files returned %d bytes of secret material", len(raw))
	}
}

func TestQueryRepublishesAMissingMetadataCopy(t *testing.T) {
	cfg := sidecarConfig(t)
	dir := t.TempDir()
	s := openSQLStore(t, dir, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"v": "a"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	copies, err := filepath.Glob(filepath.Join(dir, "ns.lakehouse", "data", "*", "metadata", currentMetadataName))
	if err != nil || len(copies) != 1 {
		t.Fatalf("copies %v %v", copies, err)
	}
	if err := os.Remove(copies[0]); err != nil {
		t.Fatal(err)
	}
	if res := mustQuery(t, s, "ns", "SELECT count(*) AS n FROM t"); res.Rows[0]["n"] != int64(1) {
		t.Fatalf("a missing metadata copy must be republished: %v", res.Rows)
	}
}

func TestQueryRefusesDuplicateColumnLabels(t *testing.T) {
	cfg := sidecarConfig(t)
	s := openSQLStore(t, t.TempDir(), cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "ns", "SELECT 1 AS a, 2 AS a", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a duplicate label must be refused, not silently collapsed: %v", err)
	}
}

func TestDropWaitsForAnInFlightQueryOutsideTheStoreLock(t *testing.T) {
	cfg := sidecarConfig(t)
	dir := t.TempDir()
	s := openSQLStore(t, dir, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"v": "a"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	mustQuery(t, s, "ns", "SELECT count(*) AS n FROM t")
	tables, err := filepath.Glob(filepath.Join(dir, "ns.lakehouse", "data", "t-*"))
	if err != nil || len(tables) != 1 {
		t.Fatalf("table directories %v %v", tables, err)
	}
	sc := s.namespaces["ns"].sql
	sc.run.Lock()
	dropped := make(chan error, 1)
	go func() { dropped <- s.DropTable(ctx, "ns", "t", store.Incarnation{}) }()
	for {
		names, err := s.ListTables(ctx, "ns", nil)
		if err != nil {
			sc.run.Unlock()
			t.Fatal(err)
		}
		if len(names) == 0 {
			break
		}
		runtime.Gosched()
	}
	_, statErr := os.Stat(tables[0])
	sc.run.Unlock()
	if statErr != nil {
		t.Fatalf("drop removed the table's files under a running query: %v", statErr)
	}
	if err := <-dropped; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tables[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drop left the table's files once the query finished: %v", err)
	}
}

func TestDropNamespaceWaitsForAnInFlightQuery(t *testing.T) {
	cfg := sidecarConfig(t)
	dir := t.TempDir()
	s := openSQLStore(t, dir, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"v": "a"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	mustQuery(t, s, "ns", "SELECT count(*) AS n FROM t")
	sc := s.namespaces["ns"].sql
	sc.run.Lock()
	dropped := make(chan error, 1)
	go func() { dropped <- s.DropNamespace(ctx, "ns", [16]byte{}) }()
	select {
	case err := <-dropped:
		sc.run.Unlock()
		t.Fatalf("drop_namespace finished under a running query: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	_, statErr := os.Stat(filepath.Join(dir, "ns.lakehouse"))
	sc.run.Unlock()
	if statErr != nil {
		t.Fatalf("drop_namespace removed files under a running query: %v", statErr)
	}
	if err := <-dropped; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ns.lakehouse")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drop_namespace left the namespace once the query finished: %v", err)
	}
}
