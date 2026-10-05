package store

import (
	"database/sql"
	"github.com/lsm/dolmen/internal/schema"
	"path/filepath"
	"testing"
)

func TestSQLiteCommitCatalogUpgradePreservesLegacyChanges(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Type: schema.Text}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	insert := func(body string) {
		t.Helper()
		if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": body}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	insert("legacy")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "app.db"), false))
	if err != nil {
		t.Fatal(err)
	}
	var exists int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('_dolmen_changes') WHERE name='commit_id'`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		if _, err := db.Exec(`ALTER TABLE _dolmen_changes DROP COLUMN commit_id`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`DELETE FROM _dolmen_meta WHERE key='next_commit'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE _dolmen_meta SET value=? WHERE key=?`, []byte("4"), catalogFormatKey); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if _, err := s.ListTables(ctx, "app", nil); err != nil {
		t.Fatal(err)
	}
	if format, _ := catalogMeta(t, dir, "app", catalogFormatKey); format != "5" {
		t.Fatalf("format %q, want 5", format)
	}
	if min, _ := catalogMeta(t, dir, "app", catalogMinReaderKey); min != "3" {
		t.Fatalf("minimum reader %q, want 3", min)
	}
	n, err := s.ns("app")
	if err != nil {
		t.Fatal(err)
	}
	var legacy sql.NullInt64
	err = n.ro.QueryRowContext(ctx, `SELECT commit_id FROM _dolmen_changes WHERE seq=1`).Scan(&legacy)
	n.unpin()
	if err != nil || legacy.Valid {
		t.Fatalf("legacy commit %v: %v", legacy, err)
	}
	records, _, err := s.ChangesSince(ctx, "app", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil || len(records) != 1 || records[0].Commit != 0 {
		t.Fatalf("legacy feed: %v %v", records, err)
	}
	insert("new")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	insert("later")
	n, err = s.ns("app")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	var first, last int64
	if err := n.ro.QueryRowContext(ctx, `SELECT min(commit_id),max(commit_id) FROM _dolmen_changes WHERE seq>1`).Scan(&first, &last); err != nil || first <= 0 || last <= first {
		t.Fatalf("commits %d..%d: %v", first, last, err)
	}
}
