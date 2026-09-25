package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestVacuumReclaimsFreePagesAndEmptiesTheLog(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustNS(t, st, "vac")
	if _, err := st.CreateTable(ctx, "vac", "t", []schema.Field{{Name: "body", Type: schema.Text}}); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x", 32<<10)
	for i := 0; i < 64; i++ {
		if _, err := st.Insert(ctx, "vac", "t", []map[string]any{{"body": chunk}}, testEmbed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Delete(ctx, "vac", "t", "1=1", nil, DeleteOptions{Limit: 1000, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	res, err := st.Vacuum(ctx, "vac")
	if err != nil {
		t.Fatal(err)
	}
	if res.BytesBefore < 1<<20 || res.BytesAfter >= res.BytesBefore/4 {
		t.Fatalf("vacuum must shrink the file past its freed pages: before %d after %d", res.BytesBefore, res.BytesAfter)
	}
	fi, err := os.Stat(st.nsPath("vac"))
	if err != nil || fi.Size() != res.BytesAfter {
		t.Fatalf("bytes_after %d must be the file's size once the log is empty, got %v %v", res.BytesAfter, fi, err)
	}
	if wal, err := os.Stat(st.nsPath("vac") + "-wal"); err == nil && wal.Size() != 0 {
		t.Fatalf("the write-ahead log must be truncated after vacuum, it holds %d bytes", wal.Size())
	}
	if _, err := st.Insert(ctx, "vac", "t", []map[string]any{{"body": "after"}}, testEmbed); err != nil {
		t.Fatalf("the namespace must stay writable after vacuum: %v", err)
	}
}

func TestVacuumOfAMissingNamespaceIsNotFound(t *testing.T) {
	st := openStore(t)
	_, err := st.Vacuum(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "create_namespace") {
		t.Fatalf("vacuum of a missing namespace must be not_found and name create_namespace, got %v", err)
	}
	if _, statErr := os.Stat(st.nsPath("nope")); !os.IsNotExist(statErr) {
		t.Fatal("vacuum must not create the namespace")
	}
}
