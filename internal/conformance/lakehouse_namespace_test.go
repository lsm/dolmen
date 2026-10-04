package conformance

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/store"
)

func TestLakehouseNamespaceBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			open := func() namespaceEngine {
				var eng namespaceEngine
				var err error
				if backend == "lakehouse" {
					eng, err = lakehouse.Open(dir)
				} else {
					eng, err = store.Open(dir)
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { eng.Close() })
				return eng
			}
			eng := open()
			ctx := t.Context()
			if _, err := eng.NamespaceState(ctx, "missing", nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("missing namespace: %v", err)
			}
			for _, name := range []string{"", "a//b", "a/b/c/d", "Upper", "../escape", strings.Repeat("a", 65)} {
				if err := eng.CreateNamespace(ctx, name, [16]byte{}); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("invalid %q: %v", name, err)
				}
			}
			for _, name := range []string{"project", "project/team", "project/team/app", "a_b", "axb", "edge", "edge-x", "data", "data/catalog", strings.Repeat("z", 64)} {
				if err := eng.CreateNamespace(ctx, name, [16]byte{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := eng.CreateNamespace(ctx, "project", [16]byte{}); !errors.Is(err, store.ErrExists) {
				t.Fatalf("duplicate: %v", err)
			}
			for prefix, want := range map[string][]string{
				"project": {"project", "project/team", "project/team/app"},
				"a_b":     {"a_b"}, "absent": {}, "edge": {"edge"},
			} {
				got, err := eng.ListNamespaces(ctx, prefix, nil)
				if err != nil || len(got) != len(want) || len(want) > 0 && !reflect.DeepEqual(got, want) {
					t.Fatalf("prefix %q: %v want %v: %v", prefix, got, want, err)
				}
			}
			gen, err := eng.NamespaceState(ctx, "project/team/app", nil)
			if err != nil || gen == [16]byte{} {
				t.Fatalf("generation: %x %v", gen, err)
			}
			if err := eng.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.ListNamespaces(ctx, "", nil); !errors.Is(err, store.ErrClosed) {
				t.Fatalf("list after close: %v", err)
			}
			eng = open()
			again, err := eng.NamespaceState(ctx, "project/team/app", nil)
			if err != nil || gen != again {
				t.Fatalf("reopen changed generation: %x %v", again, err)
			}
			if err := eng.DropNamespace(ctx, "project", [16]byte{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("non-leaf drop: %v", err)
			}
			if err := eng.DropNamespace(ctx, "project/team/app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.NamespaceState(ctx, "project/team/app", nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("dropped namespace: %v", err)
			}
			if err := eng.DropNamespace(ctx, "project/team/app", [16]byte{}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("missing drop: %v", err)
			}
			if err := eng.CreateNamespace(ctx, "project/team/app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			next, err := eng.NamespaceState(ctx, "project/team/app", nil)
			if err != nil || next == gen || next == [16]byte{} {
				t.Fatalf("recreate reused generation: %x %v", next, err)
			}
		})
	}
}
