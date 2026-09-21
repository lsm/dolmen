package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func changeOwners(t *testing.T, st *Store, nsName string) []struct {
	Kind  string
	RowID int64
	Owner string
} {
	t.Helper()
	n, err := st.ns(nsName)
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	rows, err := n.ro.QueryContext(context.Background(),
		`SELECT kind, row_id, owner FROM _dolmen_changes ORDER BY seq`)
	if err != nil {
		t.Fatalf("read change log: %v", err)
	}
	defer rows.Close()
	var out []struct {
		Kind  string
		RowID int64
		Owner string
	}
	for rows.Next() {
		var kind string
		var rowID int64
		var owner sql.NullString
		if err := rows.Scan(&kind, &rowID, &owner); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, struct {
			Kind  string
			RowID int64
			Owner string
		}{kind, rowID, owner.String})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func seedOwnedTable(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "notes", []schema.Field{
		{Name: "sku", Type: schema.String},
		{Name: "body", Type: schema.Text},
	}, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestEveryChangeRecordCarriesTheRowsOwner(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	seedOwnedTable(t, st)

	if _, err := st.Insert(ctx, "ns", "notes", []map[string]any{{"sku": "a", "body": "alice's"}},
		WriteOpts{Owner: "alice"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("alice insert: %v", err)
	}
	if _, err := st.Insert(ctx, "ns", "notes", []map[string]any{{"sku": "b", "body": "bob's"}},
		WriteOpts{Owner: "bob"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("bob insert: %v", err)
	}
	if _, err := st.Update(ctx, "ns", "notes", "sku = ?", []any{"b"},
		map[string]any{"body": "bob's, edited"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := st.Delete(ctx, "ns", "notes", "sku = ?", []any{"a"},
		DeleteOpts{Confirm: true}, nil, Incarnation{}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got := changeOwners(t, st, "ns")
	want := []struct {
		Kind  string
		RowID int64
		Owner string
	}{
		{"insert", 1, "alice"},
		{"insert", 2, "bob"},
		{"update", 2, "bob"},
		{"delete", 1, "alice"},
	}
	if len(got) != len(want) {
		t.Fatalf("change log has %d records, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d is %v, want %v — a feed cannot be scoped by a label nobody wrote", i, got[i], want[i])
		}
	}
}

func TestAnUpsertLabelsTheRowItFoundNotTheWriter(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	seedOwnedTable(t, st)

	if _, err := st.Insert(ctx, "ns", "notes", []map[string]any{{"sku": "k", "body": "bob's"}},
		WriteOpts{Owner: "bob"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("bob insert: %v", err)
	}
	if _, err := st.UpsertByKey(ctx, "ns", "notes", []string{"sku"},
		[]map[string]any{{"sku": "k", "body": "alice edited it"}},
		WriteOpts{Owner: "alice"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("alice upsert: %v", err)
	}

	got := changeOwners(t, st, "ns")
	if len(got) != 2 {
		t.Fatalf("want an insert and an update: %v", got)
	}
	if got[1].Kind != "update" || got[1].Owner != "bob" {
		t.Fatalf("an update to bob's row must be labelled bob's, whoever wrote it: %v", got[1])
	}
}

func TestATableWithoutOwnersLabelsNothing(t *testing.T) {
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
	if _, err := st.Delete(ctx, "ns", "plain", "1=1", nil, DeleteOpts{Confirm: true}, nil, Incarnation{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, rec := range changeOwners(t, st, "ns") {
		if rec.Owner != "" {
			t.Fatalf("a table with no owner column produced a labelled change: %v", rec)
		}
	}
}
