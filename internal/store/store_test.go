package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func noteFields() []schema.Field {
	return []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true},
		{Name: "body", Type: schema.Text, Fulltext: true, Vectorize: true},
		{Name: "score", Type: schema.Number},
		{Name: "done", Type: schema.Boolean},
		{Name: "tags", Type: schema.JSON},
		{Name: "emb", Type: schema.Vector, Dim: 4},
	}
}

func mustCreateNotes(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields()); err != nil {
		t.Fatalf("create table: %v", err)
	}
}

func TestTableAndNamespaceValidation(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "BadName", noteFields()); err == nil {
		t.Fatal("expected invalid table name to be rejected")
	}
	if _, err := st.CreateTable(ctx, "../escape", "notes", noteFields()); err == nil {
		t.Fatal("expected invalid namespace to be rejected")
	}
	if _, err := st.CreateTable(ctx, "test", "notes", []schema.Field{
		{Name: "title", Type: "bogus"},
	}); err == nil {
		t.Fatal("expected invalid type to be rejected")
	}
	if _, err := st.CreateTable(ctx, "test", "notes", []schema.Field{
		{Name: "a", Type: schema.Vector},
	}); err == nil {
		t.Fatal("expected vector without dim to be rejected")
	}
	if _, _, err := st.DescribeTable(ctx, "test", "missing"); err == nil {
		t.Fatal("expected missing table to 404")
	}
}

func TestFTSSuffixAndReservedNames(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "notes__fts", noteFields()); err == nil {
		t.Fatal("expected __fts-suffixed table name to be rejected")
	}
	if _, err := st.CreateTable(ctx, "test", "ranked", []schema.Field{
		{Name: "rank", Type: schema.String, Fulltext: true},
	}); err == nil {
		t.Fatal("expected fulltext field named rank to be rejected")
	}
	if _, err := st.CreateTable(ctx, "test", "ranked", []schema.Field{
		{Name: "rank", Type: schema.String},
		{Name: "rowid_like", Type: schema.String},
	}); err != nil {
		t.Fatalf("non-fulltext rank field should be allowed: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "shadowed", []schema.Field{
		{Name: "rowid", Type: schema.String},
	}); err == nil {
		t.Fatal("expected rowid field name to be rejected")
	}
}

func TestSQLKeywordNamesRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "select", noteFields()); err == nil {
		t.Fatal("expected SQL keyword table name to be rejected")
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields()); err != nil {
		t.Fatalf("create notes: %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "order", Type: schema.String}},
	}, testEmbed, 0); err == nil {
		t.Fatal("expected SQL keyword field name to be rejected on add_field")
	}
	if _, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpRenameField, From: "title", To: "group"},
	}, testEmbed, 0); err == nil {
		t.Fatal("expected SQL keyword field name to be rejected on rename_field")
	}
	if _, err := st.CreateTable(ctx, "test", "my_select", []schema.Field{
		{Name: "order_id", Type: schema.String},
	}); err != nil {
		t.Fatalf("suggested non-keyword name should be accepted: %v", err)
	}
}

func TestLegacyKeywordTableRemainsAccessible(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open namespace: %v", err)
	}
	legacyTable := "select"
	legacyFields := []schema.Field{{Name: "order", Type: schema.String}}

	// Simulate a table created before keyword restrictions: create the SQLite
	// table directly and register it in _dolmen_tables.
	if _, err := n.rw.ExecContext(ctx, tableDDL(legacyTable, legacyFields)); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	sc := schema.TableSchema{Namespace: "test", Name: legacyTable, Version: 1, Fields: legacyFields}
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if _, err := n.rw.ExecContext(ctx,
		`INSERT INTO _dolmen_tables(name, version, schema_json) VALUES(?,?,?)`,
		legacyTable, 1, string(raw)); err != nil {
		t.Fatalf("register legacy table: %v", err)
	}

	// Operations that load the schema must succeed for the legacy table.
	if _, _, err := st.DescribeTable(ctx, "test", legacyTable); err != nil {
		t.Fatalf("describe legacy keyword table: %v", err)
	}
	ids, err := st.Insert(ctx, "test", legacyTable, []map[string]any{{"order": "value"}}, testEmbed)
	if err != nil {
		t.Fatalf("insert into legacy keyword table: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT * FROM "select" WHERE id = ?`, []any{ids[0]})
	if err != nil || len(rows) != 1 || rows[0]["order"] != "value" {
		t.Fatalf("query legacy keyword table: %v %v", err, rows)
	}
	// New tables with keyword names must still be rejected.
	if _, err := st.CreateTable(ctx, "test", "group", []schema.Field{{Name: "x", Type: schema.String}}); err == nil {
		t.Fatal("expected new keyword table name to be rejected")
	}
}

func TestLegacyKeywordFieldMigration(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open namespace: %v", err)
	}
	legacyTable := "orders"
	legacyFields := []schema.Field{{Name: "order", Type: schema.String}}
	if _, err := n.rw.ExecContext(ctx, tableDDL(legacyTable, legacyFields)); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	sc := schema.TableSchema{Namespace: "test", Name: legacyTable, Version: 1, Fields: legacyFields}
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if _, err := n.rw.ExecContext(ctx,
		`INSERT INTO _dolmen_tables(name, version, schema_json) VALUES(?,?,?)`,
		legacyTable, 1, string(raw)); err != nil {
		t.Fatalf("register legacy table: %v", err)
	}

	// Migrate should allow a keyword field to be renamed away, and should allow
	// unrelated changes to a table that contains a keyword field. The rename is
	// destructive, so it needs the expected_version precondition.
	if _, err := st.Migrate(ctx, "test", legacyTable, []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "priority", Type: schema.Number}},
		{Op: schema.OpRenameField, From: "order", To: "my_order"},
	}, testEmbed, 1); err != nil {
		t.Fatalf("migrate legacy keyword field: %v", err)
	}
	newSc, _, err := st.DescribeTable(ctx, "test", legacyTable)
	if err != nil {
		t.Fatalf("describe after migrate: %v", err)
	}
	if newSc.Field("order") != nil || newSc.Field("my_order") == nil || newSc.Field("priority") == nil {
		t.Fatalf("unexpected fields after migration: %+v", newSc.Fields)
	}

	// Rename to a keyword must still fail.
	if _, err := st.Migrate(ctx, "test", legacyTable, []schema.Change{
		{Op: schema.OpRenameField, From: "my_order", To: "group"},
	}, testEmbed, 2); err == nil {
		t.Fatal("expected rename to a keyword to fail")
	}

	// Drop the legacy keyword field should also work (uses its current name).
	if _, err := st.Migrate(ctx, "test", legacyTable, []schema.Change{
		{Op: schema.OpDropField, Name: "my_order"},
	}, testEmbed, 2); err != nil {
		t.Fatalf("drop legacy renamed field: %v", err)
	}
}

func TestFTSShadowTableNamesRejected(t *testing.T) {
	st := openStore(t)
	for _, name := range []string{"notes__fts", "notes__fts_data", "notes__fts_idx", "notes__fts_content"} {
		if _, err := st.CreateTable(context.Background(), "test", name, noteFields()); err == nil {
			t.Fatalf("expected shadow-table name %q to be rejected", name)
		}
	}
	for _, name := range []string{"fts_notes", "myfts", "notes_fts"} {
		if _, err := st.CreateTable(context.Background(), "test", name, noteFields()); err != nil {
			t.Fatalf("expected ordinary name %q to be accepted: %v", name, err)
		}
	}
}

func TestStoragePermissions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "sec", "t", []schema.Field{
		{Name: "x", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("expected data dir 0700, got %o", info.Mode().Perm())
	}
	dbInfo, err := os.Stat(filepath.Join(dir, "sec.db"))
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if dbInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected db file 0600, got %o", dbInfo.Mode().Perm())
	}
}

func TestRequiredFieldsEmitNotNull(t *testing.T) {
	ddl := tableDDL("notes", noteFields())
	if !strings.Contains(ddl, `"score" NUMERIC`) || strings.Contains(ddl, `"score" NUMERIC NOT NULL`) {
		t.Fatalf("optional field must stay nullable, got: %s", ddl)
	}
	fields := noteFields()
	fields[2].Required = true
	ddl = tableDDL("notes", fields)
	if !strings.Contains(ddl, `"score" NUMERIC NOT NULL`) {
		t.Fatalf("required field must emit NOT NULL, got: %s", ddl)
	}
}

func TestRowIdsNotReusedAfterDeleteAll(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	first, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "one"},
		{"title": "two"},
	}, testEmbed)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOptions{}); err != nil {
		t.Fatalf("delete all: %v", err)
	}
	second, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "three"},
	}, testEmbed)
	if err != nil {
		t.Fatalf("reinsert: %v", err)
	}

	var maxFirst int64
	seen := map[int64]bool{}
	for _, id := range first {
		seen[id] = true
		if id > maxFirst {
			maxFirst = id
		}
	}
	for _, id := range second {
		if seen[id] {
			t.Fatalf("id %d was reused after delete-all; prior ids %v, new ids %v", id, first, second)
		}
		if id <= maxFirst {
			t.Fatalf("id %d does not exceed the prior max id %d; ids must stay monotonic across delete-all", id, maxFirst)
		}
	}
}

func TestCreateTableTooManyFieldsRejected(t *testing.T) {
	st := openStore(t)
	fields := make([]schema.Field, MaxFieldsPerTable+1)
	for i := range fields {
		fields[i] = schema.Field{Name: fmt.Sprintf("f%d", i), Type: schema.String}
	}
	_, err := st.CreateTable(context.Background(), "test", "toomany", fields)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected field-count bound to reject with ErrInvalid, got %v", err)
	}
}
