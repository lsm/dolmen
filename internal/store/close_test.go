package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openStoreAt(t *testing.T, dir string) legacyStore {
	t.Helper()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return legacy(st)
}

func TestCloseIsIdempotent(t *testing.T) {
	st := openStore(t)
	if err := st.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second close must return the same completion result, got %v", err)
	}
}

func TestCloseIsTerminalForOperations(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := st.CreateNamespace("after"); !errors.Is(err, ErrClosed) {
		t.Fatalf("create namespace after close must fail with ErrClosed, got %v", err)
	}
	if _, err := st.ListNamespaces(); !errors.Is(err, ErrClosed) {
		t.Fatalf("list namespaces after close must fail with ErrClosed, got %v", err)
	}
	if err := st.DropNamespace("test"); !errors.Is(err, ErrClosed) {
		t.Fatalf("drop namespace after close must fail with ErrClosed, got %v", err)
	}
	if _, err := st.ListTables(ctx, "test"); !errors.Is(err, ErrClosed) {
		t.Fatalf("list tables after close must fail with ErrClosed, got %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "fresh", noteFields()); !errors.Is(err, ErrClosed) {
		t.Fatalf("create table after close must fail with ErrClosed, got %v", err)
	}
	if _, _, err := st.DescribeTable(ctx, "test", "notes"); !errors.Is(err, ErrClosed) {
		t.Fatalf("describe table after close must fail with ErrClosed, got %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes"); !errors.Is(err, ErrClosed) {
		t.Fatalf("drop table after close must fail with ErrClosed, got %v", err)
	}
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "b"}}, testEmbed); !errors.Is(err, ErrClosed) {
		t.Fatalf("insert after close must fail with ErrClosed, got %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT * FROM notes", nil, 0, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("query after close must fail with ErrClosed, got %v", err)
	}
}

func TestCloseDoesNotDeleteData(t *testing.T) {
	dir := t.TempDir()
	st := openStoreAt(t, dir)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "kept"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("close must not delete data files: %v", err)
	}
}

func TestCloseRacesListNamespaces(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "app")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_, _ = st.ListNamespaces()
		}
	}()
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	<-done
}
