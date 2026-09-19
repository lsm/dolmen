package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/postgres"
	"github.com/lsm/dolmen/internal/store"
)

type namespaceEngine interface {
	CreateNamespace(context.Context, string, [16]byte) error
	DropNamespace(context.Context, string, [16]byte) error
	NamespaceState(context.Context, string, []store.AuthBinding) ([16]byte, error)
	ListNamespaces(context.Context, string, []store.AuthBinding) ([]string, error)
	Close() error
}

func postgresNamespaceEngine(t *testing.T) namespaceEngine {
	t.Helper()
	dsn := os.Getenv("DOLMEN_TEST_PG_DSN")
	if dsn == "" {
		if os.Getenv("DOLMEN_TEST_PG_REQUIRED") == "1" {
			t.Fatal("PostgreSQL CI requires DOLMEN_TEST_PG_DSN")
		}
		t.Skip("set DOLMEN_TEST_PG_DSN for PostgreSQL namespace conformance")
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	catalog := "dolmen_conf_" + hex.EncodeToString(id[:])
	s, err := postgres.Open(t.Context(), postgres.Config{DSN: dsn, Catalog: catalog, QueryRole: os.Getenv("DOLMEN_TEST_PG_QUERY_ROLE")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer s.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		names, err := s.ListNamespaces(ctx, "", nil)
		if err != nil {
			t.Error(err)
		}
		for i := len(names) - 1; i >= 0; i-- {
			if err := s.DropNamespace(ctx, names[i], [16]byte{}); err != nil {
				t.Error(err)
			}
		}
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{catalog}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestNamespaceBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var eng namespaceEngine
			if backend == "postgres" {
				eng = postgresNamespaceEngine(t)
			} else {
				s, err := store.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				eng = s
				t.Cleanup(func() { s.Close() })
			}
			ctx := t.Context()
			if _, err := eng.NamespaceState(ctx, "missing", nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("missing: %v", err)
			}
			for _, name := range []string{"", "a//b", "a/b/c/d", "Upper", strings.Repeat("a", 65)} {
				if err := eng.CreateNamespace(ctx, name, [16]byte{}); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("invalid %q: %v", name, err)
				}
			}
			for _, name := range []string{"project", "project/team", "project/team/app", "a_b", "axb", strings.Repeat("z", 64)} {
				if err := eng.CreateNamespace(ctx, name, [16]byte{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := eng.CreateNamespace(ctx, "project", [16]byte{}); !errors.Is(err, store.ErrExists) {
				t.Fatalf("duplicate: %v", err)
			}
			for prefix, want := range map[string][]string{
				"project": {"project", "project/team", "project/team/app"},
				"a_b":     {"a_b"}, "absent": {},
			} {
				got, err := eng.ListNamespaces(ctx, prefix, nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != len(want) || len(want) > 0 && !reflect.DeepEqual(got, want) {
					t.Fatalf("prefix %q: %v want %v", prefix, got, want)
				}
			}
			gen, err := eng.NamespaceState(ctx, "project/team/app", nil)
			if err != nil || gen == [16]byte{} {
				t.Fatalf("generation: %x %v", gen, err)
			}
			again, err := eng.NamespaceState(ctx, "project/team/app", nil)
			if err != nil || gen != again {
				t.Fatalf("generation unstable: %v", err)
			}
			if err := eng.DropNamespace(ctx, "project", [16]byte{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("non-leaf drop: %v", err)
			}
			if err := eng.DropNamespace(ctx, "project/team/app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if err := eng.CreateNamespace(ctx, "project/team/app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			next, err := eng.NamespaceState(ctx, "project/team/app", nil)
			if err != nil || next == gen {
				t.Fatalf("recreate reused generation: %v", err)
			}
		})
	}
}
