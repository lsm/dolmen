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

func ownersOf(t *testing.T, st *Store, want int) map[string]string {
	t.Helper()
	rows, err := st.GetRows(context.Background(), "ns", "notes", []int64{1, 2, 3}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows.Rows) != want {
		t.Fatalf("the table holds %d rows, want %d", len(rows.Rows), want)
	}
	out := map[string]string{}
	for _, r := range rows.Rows {
		owner, _ := r[schema.OwnerColumn].(string)
		body, _ := r["body"].(string)
		out[owner+"/"+body] = body
	}
	return out
}

func TestAScopedUpdateTouchesOnlyOwnRows(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	res, err := st.Update(ctx, "ns", "notes", "1=1", nil, map[string]any{"body": "rewritten"},
		Embedder{}, &RowScope{Owner: "alice"}, Incarnation{})
	if err != nil {
		t.Fatalf("a scoped update was refused: %v", err)
	}
	if res.Updated != 2 {
		t.Fatalf("a scoped update reported %d rows, want alice's 2", res.Updated)
	}
	got := ownersOf(t, st, 3)
	if _, ok := got["bob/beta note"]; !ok {
		t.Fatalf("a scoped update rewrote a foreign row: %v", got)
	}
}

func TestAScopedDeleteTouchesOnlyOwnRows(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	res, err := st.Delete(ctx, "ns", "notes", "1=1", nil, DeleteOpts{Confirm: true},
		&RowScope{Owner: "alice"}, Incarnation{})
	if err != nil {
		t.Fatalf("a scoped delete was refused: %v", err)
	}
	if res.Deleted != 2 {
		t.Fatalf("a scoped delete removed %d rows, want alice's 2", res.Deleted)
	}
	got := ownersOf(t, st, 1)
	if _, ok := got["bob/beta note"]; !ok {
		t.Fatalf("a scoped delete removed a foreign row: %v", got)
	}
}

func TestAScopedFilterNeverRunsAgainstAForeignRow(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()
	const oracle = `iif(body = 'beta note', abs(-9223372036854775808), 1) = 1`

	res, err := st.Delete(ctx, "ns", "notes", oracle, nil, DeleteOpts{DryRun: true},
		&RowScope{Owner: "alice"}, Incarnation{})
	if err != nil {
		t.Fatalf("a filter that would overflow on bob's row reached it, so its error reports what bob's row holds: %v", err)
	}
	if res.Matched != 2 {
		t.Fatalf("the scoped filter matched %d rows, want alice's 2", res.Matched)
	}

	if _, err := st.Delete(ctx, "ns", "notes", oracle, nil, DeleteOpts{DryRun: true}, nil, Incarnation{}); err == nil {
		t.Fatal("the same filter must still raise unscoped, or this test proves nothing about the boundary")
	}
}

func TestAScopedSearchFiltersOverOwnRowsOnly(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	res, err := st.SearchFulltext(ctx, "ns", "notes", "note", "body IS NOT NULL", nil, false,
		&RowScope{Owner: "alice"}, Incarnation{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("a scoped filtered search was refused: %v", err)
	}
	for _, r := range res.Rows {
		if body, _ := r["body"].(string); body == "beta note" {
			t.Fatalf("a scoped filtered search returned a foreign row: %v", res.Rows)
		}
	}
	if len(res.Rows) != 1 {
		t.Fatalf("the scoped search returned %d rows, want alice's 1 matching note", len(res.Rows))
	}
}

func TestScopeAndFilterArgumentsBindInOrder(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	res, err := st.SearchFulltext(ctx, "ns", "notes", "alpha", "body LIKE ? AND body <> ?",
		[]any{"%alpha%", "nothing"}, false, &RowScope{Owner: "alice"}, Incarnation{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("a scoped search carrying filter arguments failed, so the owner and the filter values bound to the wrong places: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("the scoped filtered search returned %d rows, want alice's 2: %v", len(res.Rows), res.Rows)
	}
	for _, r := range res.Rows {
		if owner, _ := r[schema.OwnerColumn].(string); owner != "alice" {
			t.Fatalf("a scoped filtered search returned a row owned by %q: %v", owner, r)
		}
	}

	swapped, err := st.SearchFulltext(ctx, "ns", "notes", "alpha", "body LIKE ? AND body <> ?",
		[]any{"%alpha%", "alpha note"}, false, &RowScope{Owner: "alice"}, Incarnation{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("second scoped search: %v", err)
	}
	if len(swapped.Rows) != 1 {
		t.Fatalf("the second filter argument was not honoured: %d rows, want 1: %v", len(swapped.Rows), swapped.Rows)
	}

	del, err := st.Delete(ctx, "ns", "notes", "body LIKE ? AND id > ?", []any{"%alpha%", 0},
		DeleteOpts{DryRun: true}, &RowScope{Owner: "alice"}, Incarnation{})
	if err != nil {
		t.Fatalf("a scoped delete carrying filter arguments failed: %v", err)
	}
	if del.Matched != 2 {
		t.Fatalf("the scoped dry run matched %d rows, want alice's 2", del.Matched)
	}
}

func TestAScopedUpsertInsertsRatherThanTouchingAForeignMatch(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	res, err := st.Upsert(ctx, "ns", "notes", "body = ?", []any{"beta note"},
		map[string]any{"body": "beta note"}, WriteOpts{Owner: "alice"}, Embedder{},
		&RowScope{Owner: "alice"}, Incarnation{})
	if err != nil {
		t.Fatalf("a scoped upsert was refused: %v", err)
	}
	if res.Updated != 0 || res.Inserted != 1 {
		t.Fatalf("a scoped upsert matching only a foreign row reported %d updated and %d inserted, want 0 and 1", res.Updated, res.Inserted)
	}
	rows, err := st.GetRows(ctx, "ns", "notes", []int64{3}, nil, Incarnation{})
	if err != nil || len(rows.Rows) != 1 {
		t.Fatalf("read back bob's row: %v", err)
	}
	if owner, _ := rows.Rows[0][schema.OwnerColumn].(string); owner != "bob" {
		t.Fatalf("the upsert took over a foreign row instead of inserting: %v", rows.Rows[0])
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

func TestMigrationPreservesRowAccess(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	if _, err := st.Migrate(ctx, "ns", "notes", []schema.Change{
		{Op: "add_field", Field: &schema.Field{Name: "tag", Type: schema.String}},
	}, Embedder{}, Incarnation{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sc, _, err := st.TableState(ctx, "ns", "notes", nil)
	if err != nil {
		t.Fatalf("table state: %v", err)
	}
	if !sc.HasOwner || sc.RowAccess != schema.RowAccessOwn {
		t.Fatalf("a migration stripped the row_access annotation while the owner column persists: %+v", sc)
	}

	own, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3}, &RowScope{Owner: "alice"}, Incarnation{})
	if err != nil {
		t.Fatalf("scoped read after migration: %v", err)
	}
	if len(own.Rows) != 2 {
		t.Fatalf("after a migration the scope stopped filtering: %d rows", len(own.Rows))
	}
}

func TestScopedUpsertByKeyCannotTouchAForeignRow(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "docs", []schema.Field{
		{Name: "sku", Type: schema.String},
		{Name: "body", Type: schema.Text},
	}, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "ns", "docs", []map[string]any{{"sku": "k1", "body": "bob's"}},
		WriteOpts{Owner: "bob"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := st.UpsertByKey(ctx, "ns", "docs", []string{"sku"},
		[]map[string]any{{"sku": "k1", "body": "alice overwrote it"}},
		WriteOpts{Owner: "alice"}, Embedder{}, &RowScope{Owner: "alice"}, Incarnation{}); err == nil {
		t.Fatal("a scoped upsert_by_key matched a row owned by someone else")
	}

	rows, err := st.GetRows(ctx, "ns", "docs", []int64{1}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows.Rows) != 1 || rows.Rows[0]["body"] != "bob's" {
		t.Fatalf("bob's row was modified: %v", rows.Rows)
	}
}

func TestScopedVectorSearchKeepsBothItsScopeAndItsFilterArguments(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "docs", []schema.Field{
		{Name: "body", Type: schema.Text},
		{Name: "emb", Type: schema.Vector, Dim: 4},
	}, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, r := range []struct {
		owner, body string
		vec         []float32
	}{
		{"alice", "alpha", []float32{1, 0, 0, 0}},
		{"alice", "beta", []float32{0, 1, 0, 0}},
		{"bob", "alpha", []float32{1, 0, 0, 0}},
	} {
		if _, err := st.Insert(ctx, "ns", "docs", []map[string]any{{"body": r.body, "emb": r.vec}},
			WriteOpts{Owner: r.owner}, Embedder{}, nil, Incarnation{}); err != nil {
			t.Fatalf("insert for %s: %v", r.owner, err)
		}
	}

	res, err := st.SearchVector(ctx, "ns", "docs", VectorQuery{
		Column: "emb",
		Vec:    []float32{1, 0, 0, 0},
		Filter: "body = ?",
		Args:   []any{"alpha"},
	}, false, &RowScope{Owner: "alice"}, Incarnation{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("a scoped vector search carrying a filter argument failed, so the owner and the filter value bound to the wrong places: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("the scoped vector search returned %d rows, want alice's 1 alpha row: %v", len(res.Rows), res.Rows)
	}
	if owner, _ := res.Rows[0][schema.OwnerColumn].(string); owner != "alice" {
		t.Fatalf("a scoped vector search returned a row owned by %q: %v", owner, res.Rows[0])
	}
}
