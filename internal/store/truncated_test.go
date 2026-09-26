package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func seedTwoNamespaces(t *testing.T, dir string) {
	t.Helper()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	l := legacy(st)
	ctx := context.Background()
	for _, ns := range []string{"broken", "whole"} {
		if err := l.CreateNamespace(ns); err != nil {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
		if _, err := l.CreateTable(ctx, ns, "t", []schema.Field{{Name: "k", Type: schema.Number}}); err != nil {
			t.Fatalf("create table in %s: %v", ns, err)
		}
		if _, err := l.Insert(ctx, ns, "t", []map[string]any{{"k": 1}}, testEmbed); err != nil {
			t.Fatalf("insert into %s: %v", ns, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func TestReopeningATruncatedNamespaceRefusesItAndSaysWhatToDo(t *testing.T) {
	for _, size := range []string{"zero", "one byte", "half"} {
		t.Run(size, func(t *testing.T) {
			dir := t.TempDir()
			seedTwoNamespaces(t, dir)
			path := filepath.Join(dir, "broken.db")
			full, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			keep := path + ".keep"
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keep, original, 0o600); err != nil {
				t.Fatal(err)
			}
			switch size {
			case "zero":
				err = os.Truncate(path, 0)
			case "one byte":
				err = os.Truncate(path, 1)
			default:
				err = os.Truncate(path, full.Size()/2)
			}
			if err != nil {
				t.Fatalf("truncate: %v", err)
			}
			for _, sidecar := range []string{"broken.db-wal", "broken.db-shm"} {
				if err := os.Remove(filepath.Join(dir, sidecar)); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}

			var logged bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
			st, err := Open(dir)
			if err != nil {
				slog.SetDefault(old)
				t.Fatalf("reopening a truncated namespace must not fail the data directory: %v", err)
			}
			got := logged.String()
			slog.SetDefault(old)
			t.Cleanup(func() { st.Close() })
			if !strings.Contains(got, "namespace is unreadable") || !strings.Contains(got, "namespace=broken") {
				t.Fatalf("startup must name the unreadable namespace, logged %q", got)
			}
			if strings.Contains(got, "namespace=whole") {
				t.Fatalf("a healthy namespace must not be reported unreadable, logged %q", got)
			}

			ctx := context.Background()
			if _, err := st.GetRows(ctx, "broken", "t", []int64{1}, nil, Incarnation{}); !errors.Is(err, ErrNamespaceUnreadable) {
				t.Fatalf("read_rows on a truncated namespace = %v, want ErrNamespaceUnreadable", err)
			}
			_, err = st.CreateTable(ctx, "broken", "t2", []schema.Field{{Name: "k", Type: schema.Number}}, TableOpts{}, [16]byte{})
			if !errors.Is(err, ErrNamespaceUnreadable) {
				t.Fatalf("create_table on a truncated namespace = %v, want ErrNamespaceUnreadable: a truncated file must not be re-initialized and the data silently lost", err)
			}
			if err := st.CreateNamespace(ctx, "broken", [16]byte{}); !errors.Is(err, ErrNamespaceUnreadable) {
				t.Fatalf("create_namespace over a truncated namespace = %v, want ErrNamespaceUnreadable", err)
			}
			if err := st.Ready(ctx); err == nil {
				t.Error("readiness must report the unreadable namespace")
			}
			names, err := st.ListNamespaces(ctx, "", nil)
			if err != nil {
				t.Fatalf("listing must still work so the operator can see it: %v", err)
			}
			if len(names) != 2 {
				t.Fatalf("list = %v, want both namespaces visible", names)
			}

			if _, err := st.GetRows(ctx, "whole", "t", []int64{1}, nil, Incarnation{}); err != nil {
				t.Fatalf("the other namespace must keep working: %v", err)
			}
			if _, err := st.Insert(ctx, "whole", "t", []map[string]any{{"k": 2}}, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
				t.Fatalf("writing the other namespace must keep working: %v", err)
			}
			rows, err := st.GetRows(ctx, "whole", "t", []int64{1, 2}, nil, Incarnation{})
			if err != nil || len(rows.Rows) != 2 {
				t.Fatalf("read back the other namespace: %v %v", rows, err)
			}

			first := firstError(t, st, ctx, "broken")
			for _, want := range []string{"broken", "restore it from a backup", "unaffected"} {
				if !strings.Contains(first, want) {
					t.Fatalf("the refusal must say %q: %q", want, first)
				}
			}
			if strings.Contains(first, ".db") || strings.Contains(first, string(filepath.Separator)) {
				t.Fatalf("the refusal must not quote a file path: %q", first)
			}

			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			rows, err = st.GetRows(ctx, "broken", "t", []int64{1}, nil, Incarnation{})
			if err != nil {
				t.Fatalf("a restored file must serve again without a restart: %v", err)
			}
			if len(rows.Rows) != 1 {
				t.Fatalf("the restored row is %v, want the one that was written", rows.Rows)
			}
			if err := st.Ready(ctx); err != nil {
				t.Fatalf("readiness must recover once the file is back: %v", err)
			}
		})
	}
}

func TestATruncatedNamespaceCanStillBeDropped(t *testing.T) {
	dir := t.TempDir()
	seedTwoNamespaces(t, dir)
	path := filepath.Join(dir, "broken.db")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.DropNamespace(context.Background(), "broken", [16]byte{}); err != nil {
		t.Fatalf("dropping an unreadable namespace is the way out, so it must work: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file must be gone, stat gave %v", err)
	}
	names, err := st.ListNamespaces(context.Background(), "", nil)
	if err != nil || len(names) != 1 || names[0] != "whole" {
		t.Fatalf("list after the drop = %v (%v), want just whole", names, err)
	}
	if err := st.Ready(context.Background()); err != nil {
		t.Fatalf("readiness must recover once the namespace is gone: %v", err)
	}
}

func firstError(t *testing.T, st *Store, ctx context.Context, ns string) string {
	t.Helper()
	_, err := st.GetRows(ctx, ns, "t", []int64{1}, nil, Incarnation{})
	if err == nil {
		t.Fatal("expected the truncated namespace to be refused")
	}
	return err.Error()
}
