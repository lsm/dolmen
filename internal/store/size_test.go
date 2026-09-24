package store

import (
	"context"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestANamespaceStopsGrowingAtItsSizeLimit(t *testing.T) {
	st, err := Open(t.TempDir(), WithMaxNamespaceSize(512<<10))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := legacy(st)
	ctx := context.Background()
	mustNS(t, l, "cap")
	if _, err := l.CreateTable(ctx, "cap", "t", []schema.Field{{Name: "body", Type: schema.Text}}); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x", 32<<10)
	var full error
	for i := 0; i < 64 && full == nil; i++ {
		_, full = l.Insert(ctx, "cap", "t", []map[string]any{{"body": chunk}}, testEmbed)
	}
	if full == nil || !strings.Contains(full.Error(), "database or disk is full") {
		t.Fatalf("a namespace past its size limit must refuse the write, got %v", full)
	}
	rows, _, err := l.Query(ctx, "cap", "SELECT count(*) AS n FROM t", nil, 0, 1)
	if err != nil || rows[0]["n"].(int64) < 1 {
		t.Fatalf("what was written before the limit must stay readable: %v %v", rows, err)
	}
}

func TestTheWriteAheadLogIsTrimmedAfterCheckpoints(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "wal")
	n, err := st.ns("wal")
	if err != nil {
		t.Fatal(err)
	}
	var limit int64
	if err := n.rw.QueryRow(`PRAGMA journal_size_limit`).Scan(&limit); err != nil || limit != walSizeLimit {
		t.Fatalf("journal_size_limit %d (%v), want %d", limit, err, walSizeLimit)
	}
}

func TestParseSize(t *testing.T) {
	for raw, want := range map[string]int64{"0": 0, "4096": 4096, "10GiB": 10 << 30, "512 MiB": 512 << 20, "3KiB": 3 << 10} {
		if got, err := ParseSize(raw); err != nil || got != want {
			t.Fatalf("ParseSize(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"-1", "10GB", "lots", "1.5GiB"} {
		if _, err := ParseSize(raw); err == nil {
			t.Fatalf("ParseSize(%q) accepted", raw)
		}
	}
}

func TestOpenRefusesANegativeSizeLimit(t *testing.T) {
	if _, err := Open(t.TempDir(), WithMaxNamespaceSize(-1)); err == nil {
		t.Fatal("a negative size limit must be refused")
	}
}
