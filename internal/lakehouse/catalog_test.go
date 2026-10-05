package lakehouse

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
	"github.com/parquet-go/parquet-go"
)

func openStore(t *testing.T, dir string, opts ...OpenOption) *Store {
	t.Helper()
	s, err := Open(dir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestNativeCatalogIsSeparateDurableAndReadable(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, WithMaxOpenNamespaces(1))
	ctx := t.Context()
	for _, name := range []string{"project/team", "sibling"} {
		if err := s.CreateNamespace(ctx, name, [16]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	ident := table.Identifier{"project", "team", "probe"}
	var dataPath string
	err := s.withNamespace(ctx, "project/team", func(n *namespace) error {
		if n.catalog.CatalogType() != catalog.SQL {
			t.Fatal("namespace is not an Iceberg SQL catalog")
		}
		schema := iceberg.NewSchema(0, iceberg.NestedField{ID: 1, Name: "id", Type: iceberg.PrimitiveTypes.Int64, Required: true})
		tbl, err := n.catalog.CreateTable(ctx, ident, schema, catalog.WithLocation(fileLocation(filepath.Join(n.dataDir, "probe"))), catalog.WithProperties(iceberg.Properties{"format-version": "2"}))
		if err != nil {
			return err
		}
		asch := arrow.NewSchema([]arrow.Field{{Name: "id", Type: arrow.PrimitiveTypes.Int64}}, nil)
		builder := array.NewRecordBuilder(memory.NewGoAllocator(), asch)
		defer builder.Release()
		builder.Field(0).(*array.Int64Builder).Append(42)
		record := builder.NewRecordBatch()
		defer record.Release()
		reader, err := array.NewRecordReader(asch, []arrow.Record{record})
		if err != nil {
			return err
		}
		defer reader.Release()
		tbl, err = tbl.Append(ctx, reader, nil)
		if err != nil {
			return err
		}
		files, err := tbl.Scan().PlanFiles(ctx)
		if err != nil {
			return err
		}
		if len(files) != 1 {
			t.Fatalf("catalog fixture has %d data files", len(files))
		}
		dataPath = localPath(files[0].File.FilePath())
		info, err := os.Stat(dataPath)
		if err != nil {
			return err
		}
		if files[0].File.FileSizeBytes() != info.Size() {
			t.Fatalf("manifest size %d differs from real size %d", files[0].File.FileSizeBytes(), info.Size())
		}
		if rel, err := filepath.Rel(n.dataDir, dataPath); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("data file outside namespace data directory: %s", dataPath)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.withNamespace(ctx, "sibling", func(n *namespace) error {
		if _, err := n.catalog.LoadTable(ctx, ident); !errors.Is(err, catalog.ErrNoSuchTable) {
			t.Fatalf("sibling sees another catalog's table: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir)
	if err := s.withNamespace(ctx, "project/team", func(n *namespace) error {
		tbl, err := n.catalog.LoadTable(ctx, ident)
		if err != nil {
			return err
		}
		files, err := tbl.Scan().PlanFiles(ctx)
		if err != nil {
			return err
		}
		if len(files) != 1 || files[0].File.FilePath() != fileLocation(dataPath) {
			t.Fatal("catalog lost the committed data file on reopen")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type row struct {
		ID int64 `parquet:"id"`
	}
	r := parquet.NewGenericReader[row](f)
	defer r.Close()
	rows := make([]row, 2)
	n, err := r.Read(rows)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if n != 1 || rows[0].ID != 42 {
		t.Fatalf("Parquet read-back: %d %v", n, rows)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, "project/team", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("drop retained Parquet data: %v", err)
	}
	if _, err := s.NamespaceState(ctx, "sibling", nil); err != nil {
		t.Fatal(err)
	}
}

func TestLifetimeChecksAndUnsupportedAuthorization(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "parent", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	gen, err := s.NamespaceState(ctx, "parent", nil)
	if err != nil {
		t.Fatal(err)
	}
	wrong := gen
	wrong[0] ^= 1
	if err := s.CreateNamespace(ctx, "parent/child", wrong); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale parent: %v", err)
	}
	if err := s.CreateNamespace(ctx, "parent/child", gen); err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, "parent/child", wrong); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale drop: %v", err)
	}
	bindings := []store.AuthBinding{{Root: true}}
	if _, err := s.NamespaceState(ctx, "parent", bindings); !errors.Is(err, derr.ErrForbidden) {
		t.Fatalf("authorization state: %v", err)
	}
	if _, err := s.ListNamespaces(ctx, "", bindings); !errors.Is(err, derr.ErrForbidden) {
		t.Fatalf("authorization listing: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "after", [16]byte{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("create after close: %v", err)
	}
	if err := s.DropNamespace(ctx, "parent", [16]byte{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("drop after close: %v", err)
	}
	if _, err := s.NamespaceState(ctx, "parent", nil); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("state after close: %v", err)
	}
}

func TestConcurrentCreateHasOneWinner(t *testing.T) {
	s := openStore(t, t.TempDir())
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { errs <- s.CreateNamespace(t.Context(), "shared", [16]byte{}) })
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, store.ErrExists) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d create winners", winners)
	}
}

func TestCanceledOperationsDoNotCreate(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.CreateNamespace(ctx, "canceled", [16]byte{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create: %v", err)
	}
	names, err := s.ListNamespaces(t.Context(), "", nil)
	if err != nil || len(names) != 0 {
		t.Fatalf("canceled create published a namespace: %v %v", names, err)
	}
}

func TestOneOwnerPerCanonicalDirectory(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if second, err := Open(filepath.Join(dir, ".")); err == nil {
		second.Close()
		t.Fatal("duplicate ownership accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	openStore(t, dir)
}

func TestCatalogVersionGuardDoesNotInitializeOrRewrite(t *testing.T) {
	for _, tc := range []struct {
		name, update string
		want         error
	}{
		{"newer", `UPDATE _dolmen_lakehouse_meta SET format = ` + strconv.Itoa(catalogFormat+1), store.ErrCatalogTooNew},
		{"zero", `UPDATE _dolmen_lakehouse_meta SET format = 0`, store.ErrCatalogCorrupt},
		{"wrong-name", `UPDATE _dolmen_lakehouse_meta SET namespace = 'other'`, store.ErrCatalogCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir)
			if err := s.CreateNamespace(t.Context(), "probe", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			gen, err := s.NamespaceState(t.Context(), "probe", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.withNamespace(t.Context(), "probe", func(n *namespace) error { _, err := n.db.ExecContext(t.Context(), tc.update); return err }); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openStore(t, dir)
			if _, err := s.NamespaceState(t.Context(), "probe", nil); !errors.Is(err, tc.want) {
				t.Fatalf("catalog guard: %v", err)
			}
			if err := s.DropNamespace(t.Context(), "probe", gen); !errors.Is(err, tc.want) {
				t.Fatalf("pinned drop bypassed unreadable generation: %v", err)
			}
			if err := s.DropNamespace(t.Context(), "probe", [16]byte{}); err != nil {
				t.Fatalf("unreadable namespace recovery drop: %v", err)
			}
		})
	}
}

func TestSymlinkNamespacePathsAreRefused(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	s := openStore(t, dir)
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := s.CreateNamespace(t.Context(), "escape/child", [16]byte{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("symlink parent create: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "target.lakehouse")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NamespaceState(t.Context(), "target", nil); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("symlink namespace open: %v", err)
	}
	names, err := s.ListNamespaces(t.Context(), "", nil)
	if err != nil || len(names) != 0 {
		t.Fatalf("listing followed symlink: %v %v", names, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink escape wrote outside: %v %v", entries, err)
	}
}

func TestIncompleteNamespacesCanBeListedAroundAndRemoved(t *testing.T) {
	for _, missing := range []string{"catalog.db", "data"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir)
			for _, name := range []string{"healthy", "partial", "partial/child"} {
				if err := s.CreateNamespace(t.Context(), name, [16]byte{}); err != nil {
					t.Fatal(err)
				}
			}
			gen, err := s.NamespaceState(t.Context(), "partial", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(dir, "partial.lakehouse", missing)); err != nil {
				t.Fatal(err)
			}
			s = openStore(t, dir)
			if _, err := s.NamespaceState(t.Context(), "partial", nil); !errors.Is(err, store.ErrCatalogCorrupt) {
				t.Fatalf("incomplete namespace: %v", err)
			}
			for prefix, want := range map[string][]string{"": {"healthy", "partial/child"}, "partial": {"partial/child"}} {
				names, err := s.ListNamespaces(t.Context(), prefix, nil)
				if err != nil || !reflect.DeepEqual(names, want) {
					t.Fatalf("list around incomplete namespace: %v want %v: %v", names, want, err)
				}
			}
			if err := s.DropNamespace(t.Context(), "partial", gen); !errors.Is(err, store.ErrCatalogCorrupt) {
				t.Fatalf("pinned drop of incomplete namespace: %v", err)
			}
			if err := s.DropNamespace(t.Context(), "partial", [16]byte{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("incomplete parent with live child: %v", err)
			}
			if err := s.DropNamespace(t.Context(), "partial/child", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if err := s.DropNamespace(t.Context(), "partial", [16]byte{}); err != nil {
				t.Fatalf("incomplete namespace recovery drop: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "partial.lakehouse")); !os.IsNotExist(err) {
				t.Fatalf("incomplete directory retained: %v", err)
			}
			if err := s.CreateNamespace(t.Context(), "partial", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			next, err := s.NamespaceState(t.Context(), "partial", nil)
			if err != nil || next == gen {
				t.Fatalf("recovery recreate lifetime: %x %v", next, err)
			}
		})
	}
}

func TestFileLocationsRoundTripOnEveryPlatform(t *testing.T) {
	for _, c := range []struct{ uri, path string }{
		{"file:///C:/data/ns.lakehouse", "C:/data/ns.lakehouse"},
		{"C:/data/ns.lakehouse", "C:/data/ns.lakehouse"},
		{"file:///tmp/data", "/tmp/data"},
		{`\C:\data\x.parquet`, "C:/data/x.parquet"},
	} {
		if got := strings.ReplaceAll(filepath.ToSlash(localPath(c.uri)), `\`, "/"); got != c.path {
			t.Fatalf("localPath(%q) = %q, want %q", c.uri, got, c.path)
		}
	}
	dir := t.TempDir()
	if got := localPath(fileLocation(dir)); got != dir {
		t.Fatalf("a location must round-trip: %q -> %q", dir, got)
	}
}
