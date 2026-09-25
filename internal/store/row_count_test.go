package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func seedCountedTable(t *testing.T, dir string, rows int) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.CreateNamespace(ctx, "rc", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "rc", "items", []schema.Field{{Name: "sku", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	records := make([]map[string]any, rows)
	for i := range records {
		records[i] = map[string]any{"sku": "x"}
	}
	if _, err := st.Insert(ctx, "rc", "items", records, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func rawNamespace(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "rc.db"), false))
	if err != nil {
		t.Fatalf("open namespace file: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func describeCount(t *testing.T, dir string) int64 {
	t.Helper()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	_, count, err := st.DescribeTable(context.Background(), "rc", "items", nil, Incarnation{})
	if err != nil {
		t.Fatalf("describe_table: %v", err)
	}
	return count
}

func TestDescribeTableReadsTheMaintainedCountInsteadOfScanning(t *testing.T) {
	dir := t.TempDir()
	seedCountedTable(t, dir, 3)
	if _, err := rawNamespace(t, dir).Exec(`UPDATE _dolmen_row_counts SET n = 41 WHERE table_name = 'items' AND scoped = 0`); err != nil {
		t.Fatalf("the namespace keeps no maintained row count: %v", err)
	}
	if got := describeCount(t, dir); got != 41 {
		t.Fatalf("describe_table reported %d; it must read the maintained count (planted as 41), not scan the table", got)
	}
}

func TestOpeningANamespaceBackfillsCountsItNeverKept(t *testing.T) {
	dir := t.TempDir()
	seedCountedTable(t, dir, 5)
	db := rawNamespace(t, dir)
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS "_dolmen_count_insert_items"`,
		`DROP TRIGGER IF EXISTS "_dolmen_count_delete_items"`,
		`DROP TABLE IF EXISTS _dolmen_row_counts`,
		`UPDATE _dolmen_meta SET value = CAST('2' AS BLOB) WHERE key IN ('catalog_format', 'catalog_min_reader')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("shape a format-2 namespace: %v", err)
		}
	}
	db.Close()

	if got := describeCount(t, dir); got != 5 {
		t.Fatalf("a namespace that kept no counts reported %d rows after reopening, want 5", got)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.Insert(ctx, "rc", "items", []map[string]any{{"sku": "y"}}, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, count, err := st.DescribeTable(ctx, "rc", "items", nil, Incarnation{}); err != nil || count != 6 {
		t.Fatalf("after the backfill an insert must be counted: got %d, %v; want 6", count, err)
	}
}
