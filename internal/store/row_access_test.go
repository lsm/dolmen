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
