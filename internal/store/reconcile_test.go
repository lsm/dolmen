package store

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestOpenNamesCatalogEntriesTheFileNoLongerBacks(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := legacy(st)
	ctx := context.Background()
	mustNS(t, l, "app")
	fields := []schema.Field{{Name: "title", Type: schema.String, Fulltext: true}, {Name: "n", Type: schema.Number}}
	for _, table := range []string{"good", "nocol", "nofts", "gone"} {
		if _, err := l.CreateTable(ctx, "app", table, fields); err != nil {
			t.Fatal(err)
		}
	}
	n, err := l.ns("app")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`ALTER TABLE nocol DROP COLUMN n`, `DROP TABLE nofts__fts`, `DROP TABLE gone`} {
		if _, err := n.rw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	n.unpin()
	st.Close()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	out := buf.String()
	for _, want := range []string{"table=nocol", "field n has no column", "table=nofts", "full-text index", "table=gone", "table itself is missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "table=good") {
		t.Errorf("an intact table must not be reported:\n%s", out)
	}
}
