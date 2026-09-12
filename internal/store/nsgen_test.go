package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNSGenStableAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustNS(t, legacy(st), "test")
	first, err := st.NamespaceState(ctx, "test", nil)
	if err != nil {
		t.Fatalf("namespace state: %v", err)
	}
	if first == ([16]byte{}) {
		t.Fatal("a minted creation id must never be the zero value — the seam's no-guard sentinel")
	}
	mustNS(t, legacy(st), "other")
	other, err := st.NamespaceState(ctx, "other", nil)
	if err != nil {
		t.Fatalf("namespace state: %v", err)
	}
	if other == first {
		t.Fatalf("two namespaces must not share a creation id: both %x", first)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err = Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	for _, ns := range []string{"test", "other"} {
		again, err := st.NamespaceState(ctx, ns, nil)
		if err != nil {
			t.Fatalf("namespace state after reopen: %v", err)
		}
		want := first
		if ns == "other" {
			want = other
		}
		if again != want {
			t.Fatalf("creation id of %s must be stable across reopen: got %x, want %x", ns, again, want)
		}
	}
}

func TestNSGenDistinctAfterDropRecreate(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustNS(t, st, "test")
	first, err := st.NamespaceState(ctx, "test", nil)
	if err != nil {
		t.Fatalf("namespace state: %v", err)
	}
	if err := st.DropNamespace("test"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := st.NamespaceState(ctx, "test", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("state after drop = %v, want ErrNotFound", err)
	}
	if err := st.CreateNamespace("test"); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	second, err := st.NamespaceState(ctx, "test", nil)
	if err != nil {
		t.Fatalf("namespace state after recreate: %v", err)
	}
	if second == first {
		t.Fatalf("a recreated namespace must mint a fresh creation id: both %x", first)
	}
	if second == ([16]byte{}) {
		t.Fatal("a minted creation id must never be the zero value — the seam's no-guard sentinel")
	}
}

func TestNamespaceStateNeverCreates(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.NamespaceState(ctx, "ghost", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("state of an absent namespace = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "ghost.db")); !os.IsNotExist(err) {
		t.Fatalf("the state read must not materialize the namespace: stat err = %v", err)
	}
	if _, err := st.NamespaceState(ctx, "a/b", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("state of an absent nested namespace = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "a")); !os.IsNotExist(err) {
		t.Fatalf("the state read must not create the missing parent directory: stat err = %v", err)
	}
	if _, err := st.NamespaceState(ctx, "A", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("state of an invalid namespace = %v, want ErrInvalid", err)
	}
}

func TestNSGenSharedDataDirectoryConverges(t *testing.T) {
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
	mustNS(t, legacy(stA), "test")
	fromA, err := stA.NamespaceState(ctx, "test", nil)
	if err != nil {
		t.Fatalf("state via A: %v", err)
	}

	fromB, err := stB.NamespaceState(ctx, "test", nil)
	if err != nil {
		t.Fatalf("state via B: %v", err)
	}
	if fromB != fromA {
		t.Fatalf("the two stores must converge on one creation id: A %x, B %x", fromA, fromB)
	}
}

func TestTableStateCarriesNSGen(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	gen, err := st.NamespaceState(ctx, "test", nil)
	if err != nil {
		t.Fatalf("namespace state: %v", err)
	}
	_, inc, err := st.TableState(ctx, "test", "notes", nil)
	if err != nil {
		t.Fatalf("table state: %v", err)
	}
	if inc.NsGen != gen {
		t.Fatalf("Incarnation.NsGen = %x, want NamespaceState's %x", inc.NsGen, gen)
	}
	if inc.Table != "notes" || inc.Version != 1 || inc.DropGen != 0 {
		t.Fatalf("Incarnation = %+v, want the table lifetime (notes, version 1, drop gen 0) alongside NsGen", inc)
	}
}
