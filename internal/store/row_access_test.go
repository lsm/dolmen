package store

import (
	"context"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func openRowAccessStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mustNS(t, legacy(st), "ns")
	return st
}

func TestOwnerColumnOnlyOnRowAccessTables(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	fields := []schema.Field{{Name: "body", Type: schema.Text}}

	plain, err := st.CreateTable(ctx, "ns", "plain", fields, TableOpts{}, [16]byte{})
	if err != nil {
		t.Fatalf("create plain: %v", err)
	}
	if plain.HasOwner || plain.RowAccess != "" {
		t.Fatalf("a default table carries owner metadata: %+v", plain)
	}

	scoped, err := st.CreateTable(ctx, "ns", "scoped", fields, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{})
	if err != nil {
		t.Fatalf("create scoped: %v", err)
	}
	if !scoped.HasOwner || scoped.RowAccess != schema.RowAccessOwn {
		t.Fatalf("a row_access table lacks owner metadata: %+v", scoped)
	}
	for _, f := range scoped.Fields {
		if f.Name == schema.OwnerColumn {
			t.Fatal("owner leaked into the declared fields list")
		}
	}
}

func TestOwnerNameIsReservedOnlyWhereTheColumnExists(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	ownerField := []schema.Field{{Name: "owner", Type: schema.String}}

	if _, err := st.CreateTable(ctx, "ns", "collide", ownerField, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err == nil {
		t.Fatal("a caller-declared owner field was accepted on a row_access table")
	}
	if _, err := st.CreateTable(ctx, "ns", "ownerok", ownerField, TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("owner must stay a valid field name on a default table, as v0.2.0 allows: %v", err)
	}
}

func TestRowAccessValueIsClosed(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	fields := []schema.Field{{Name: "body", Type: schema.Text}}
	for _, v := range []string{"everyone", "own ", "OWN", "none"} {
		if _, err := st.CreateTable(ctx, "ns", "bad", fields, TableOpts{RowAccess: v}, [16]byte{}); err == nil {
			t.Fatalf("row_access %q was accepted", v)
		}
	}
}

func TestOwnerColumnIsPhysicallyPresent(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "scoped", []schema.Field{{Name: "body", Type: schema.Text}},
		TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := st.ns("ns")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	rows, err := n.rw.QueryContext(ctx, `SELECT name FROM pragma_table_info('scoped')`)
	if err != nil {
		t.Fatalf("table info: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == schema.OwnerColumn {
			found = true
		}
	}
	if !found {
		t.Fatal("the owner column was not created")
	}
}

func seedScoped(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "body", Type: schema.Text, Fulltext: true}},
		TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, o := range []struct{ owner, body string }{
		{"alice", "alpha note"},
		{"alice", "second alpha"},
		{"bob", "beta note"},
	} {
		if _, err := st.Insert(ctx, "ns", "notes", []map[string]any{{"body": o.body}},
			WriteOpts{Owner: o.owner}, Embedder{}, nil, Incarnation{}); err != nil {
			t.Fatalf("insert for %s: %v", o.owner, err)
		}
	}
}

func TestInsertStampsTheOwner(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	n, _ := st.ns("ns")
	rows, err := n.rw.QueryContext(ctx, `SELECT owner FROM notes ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var o *string
		if err := rows.Scan(&o); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if o == nil {
			t.Fatal("a row written under auth on has a NULL owner")
		}
		owners = append(owners, *o)
	}
	if len(owners) != 3 || owners[0] != "alice" || owners[2] != "bob" {
		t.Fatalf("owners %v", owners)
	}
}

func TestDefaultTableNeverStampsAnOwner(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "plain", []schema.Field{{Name: "body", Type: schema.Text}},
		TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "ns", "plain", []map[string]any{{"body": "x"}},
		WriteOpts{Owner: "alice"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, _ := st.ns("ns")
	var cols int
	if err := n.rw.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('plain') WHERE name = 'owner'`).Scan(&cols); err != nil {
		t.Fatalf("table info: %v", err)
	}
	if cols != 0 {
		t.Fatal("an owner column exists on a default table even though the write carried an owner")
	}
}

func TestCallerCannotSupplyOwner(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	_, err := st.Insert(context.Background(), "ns", "notes",
		[]map[string]any{{"body": "x", "owner": "bob"}}, WriteOpts{Owner: "alice"}, Embedder{}, nil, Incarnation{})
	if err == nil {
		t.Fatal("a caller-supplied owner was accepted")
	}
}

func TestReadRowsHonorsTheScope(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	all, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3}, nil, Incarnation{})
	if err != nil || len(all.Rows) != 3 {
		t.Fatalf("unscoped read returned %d rows: %v", len(all.Rows), err)
	}

	own, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3}, &RowScope{Owner: "alice"}, Incarnation{})
	if err != nil {
		t.Fatalf("scoped read: %v", err)
	}
	if len(own.Rows) != 2 {
		t.Fatalf("scoped read returned %d rows, want alice's 2: %v", len(own.Rows), own.Rows)
	}
	for _, r := range own.Rows {
		if r["owner"] != "alice" {
			t.Fatalf("scoped read returned a foreign row: %v", r)
		}
	}

	empty, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3}, &RowScope{Empty: true}, Incarnation{})
	if err != nil {
		t.Fatalf("empty scope: %v", err)
	}
	if len(empty.Rows) != 0 {
		t.Fatalf("an empty scope returned %d rows", len(empty.Rows))
	}
}

func TestFulltextSearchHonorsTheScope(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	all, err := st.SearchFulltext(ctx, "ns", "notes", "note", "", nil, false, nil, Incarnation{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("unscoped search: %v", err)
	}
	if len(all.Rows) != 2 {
		t.Fatalf("unscoped search returned %d rows: %v", len(all.Rows), all.Rows)
	}

	own, err := st.SearchFulltext(ctx, "ns", "notes", "note", "", nil, false, &RowScope{Owner: "bob"}, Incarnation{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	if len(own.Rows) != 1 || own.Rows[0]["owner"] != "bob" {
		t.Fatalf("scoped search returned %v, want only bob's row", own.Rows)
	}

	empty, err := st.SearchFulltext(ctx, "ns", "notes", "note", "", nil, false, &RowScope{Empty: true}, Incarnation{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("empty scope: %v", err)
	}
	if len(empty.Rows) != 0 {
		t.Fatalf("an empty scope returned %d search rows", len(empty.Rows))
	}
}

func TestDescribeTableCountHonorsTheScope(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	for name, tc := range map[string]struct {
		scope *RowScope
		want  int64
	}{
		"unscoped":    {nil, 3},
		"alice":       {&RowScope{Owner: "alice"}, 2},
		"bob":         {&RowScope{Owner: "bob"}, 1},
		"stranger":    {&RowScope{Owner: "carol"}, 0},
		"empty scope": {&RowScope{Empty: true}, 0},
	} {
		_, count, err := st.DescribeTable(ctx, "ns", "notes", tc.scope, Incarnation{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if count != tc.want {
			t.Fatalf("%s: row_count %d, want %d", name, count, tc.want)
		}
	}
}

func TestScopedMutationsAreRefusedUntilTheFilterLanguageLands(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()
	scope := &RowScope{Owner: "alice"}

	if _, err := st.Update(ctx, "ns", "notes", "1=1", nil, map[string]any{"body": "x"}, Embedder{}, scope, Incarnation{}); err == nil {
		t.Fatal("a scoped update was executed with a caller filter")
	}
	if _, err := st.Delete(ctx, "ns", "notes", "1=1", nil, DeleteOpts{}, scope, Incarnation{}); err == nil {
		t.Fatal("a scoped delete was executed with a caller filter")
	}
	if _, err := st.Upsert(ctx, "ns", "notes", "1=1", nil, map[string]any{"body": "x"}, WriteOpts{Owner: "alice"}, Embedder{}, scope, Incarnation{}); err == nil {
		t.Fatal("a scoped upsert was executed with a caller filter")
	}
	if _, err := st.SearchFulltext(ctx, "ns", "notes", "note", "body IS NOT NULL", nil, false, scope, Incarnation{}, Page{Limit: 10}); err == nil {
		t.Fatal("a scoped filtered search was executed")
	}
}

func TestScopeOnATableWithoutOwnerIsRefused(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "plain", []schema.Field{{Name: "body", Type: schema.Text}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.GetRows(ctx, "ns", "plain", []int64{1}, &RowScope{Owner: "alice"}, Incarnation{}); err == nil {
		t.Fatal("a scope was applied to a table with no owner column, which would silently return every row")
	}
}
