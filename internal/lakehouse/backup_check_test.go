package lakehouse

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestAVacuumStampIsOddOnlyWhileAVacuumRuns(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.CreateNamespace(t.Context(), "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, namespacePath("ns"), vacuumStamp)
	read := func() int64 {
		n, err := readStamp(path)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if read() != 0 {
		t.Fatal("a namespace never vacuumed has stamp 0")
	}
	if _, err := s.Vacuum(t.Context(), "ns"); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != 2 {
		t.Fatalf("stamp after one vacuum = %d, want 2", got)
	}
	s.stampVacuum("ns", false)
	if got := read(); got != 3 {
		t.Fatalf("stamp while a vacuum runs = %d, want 3", got)
	}
	s.stampVacuum("ns", false)
	if got := read(); got != 5 {
		t.Fatalf("stamp of a vacuum after an interrupted one = %d, want 5", got)
	}
	s.stampVacuum("ns", true)
	if got := read(); got != 6 {
		t.Fatalf("stamp after it finishes = %d, want 6", got)
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		os.MkdirAll(filepath.Dir(filepath.Join(to, rel)), 0o700)
		return os.WriteFile(filepath.Join(to, rel), raw, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTheBackupCheckFindsAFileTheCopyLacks(t *testing.T) {
	live, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t, live)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"a", "b"} {
		if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"v": v}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetRows(ctx, "ns", "t", []int64{1}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(live, "ns.lakehouse", "catalog.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var loc string
	if err := db.QueryRow(`SELECT metadata_location FROM iceberg_tables`).Scan(&loc); err != nil {
		t.Fatal(err)
	}
	db.Close()
	out := t.TempDir()
	copyDir(t, live, out)
	if missing, err := unreachable(live, out, []string{loc}); err != nil || missing != "" {
		t.Fatalf("a full copy: missing %q, %v", missing, err)
	}
	var data string
	filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".parquet") {
			data = path
		}
		return err
	})
	if err := os.Remove(data); err != nil {
		t.Fatal(err)
	}
	missing, err := unreachable(live, out, []string{loc})
	if err != nil || !strings.HasSuffix(missing, filepath.Base(data)) {
		t.Fatalf("a copy without %s: missing %q, %v", data, missing, err)
	}
}
