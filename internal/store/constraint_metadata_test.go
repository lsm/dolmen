package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestConstraintMetadataCreateRefusesBeforeDDL(t *testing.T) {
	s := openRowAccessStore(t)
	fields := []schema.Field{{Name: "slug", Type: schema.String}}
	for _, opts := range []TableOpts{
		{Constraints: []schema.Constraint{{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}},
		{Constraints: []schema.Constraint{{}}},
		{AcceptReferences: true},
	} {
		if _, err := s.CreateTable(t.Context(), "ns", "notes", fields, opts, [16]byte{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsupported options returned %v", err)
		}
		tables, err := s.ListTables(t.Context(), "ns", nil)
		if err != nil || len(tables) != 0 {
			t.Fatalf("rejected options changed registry: %v, %v", tables, err)
		}
	}
	if _, err := s.CreateTable(t.Context(), "ns", "notes", fields, TableOpts{Constraints: []schema.Constraint{}}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
}

func TestConstraintMetadataSQLiteRefusesStoredDeclarations(t *testing.T) {
	for _, opts := range []TableOpts{
		{Constraints: []schema.Constraint{{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}},
		{AcceptReferences: true},
	} {
		t.Run("metadata", func(t *testing.T) {
			s := openRowAccessStore(t)
			ctx := t.Context()
			sc, err := s.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{}, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			sc.Constraints, sc.AcceptReferences = opts.Constraints, opts.AcceptReferences
			raw, err := json.Marshal(sc)
			if err != nil {
				t.Fatal(err)
			}
			n, err := s.ns("ns")
			if err != nil {
				t.Fatal(err)
			}
			defer n.unpin()
			if _, err := n.rw.ExecContext(ctx, `UPDATE _dolmen_tables SET schema_json = ? WHERE name = 'notes'`, string(raw)); err != nil {
				t.Fatal(err)
			}
			_, _, err = s.DescribeTable(ctx, "ns", "notes", nil, Incarnation{})
			if !errors.Is(err, ErrCatalogTooNew) {
				t.Fatalf("describe ignored metadata: %v", err)
			}
			_, err = s.Insert(ctx, "ns", "notes", []map[string]any{{"slug": "x"}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
			if !errors.Is(err, ErrCatalogTooNew) {
				t.Fatalf("insert ignored metadata: %v", err)
			}
			if err := s.DropTable(ctx, "ns", "notes", Incarnation{}); !errors.Is(err, ErrCatalogTooNew) {
				t.Fatalf("drop ignored metadata: %v", err)
			}
			var count int
			if err := n.rw.QueryRowContext(ctx, `SELECT count(*) FROM notes`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("refused operations changed rows: %d, %v", count, err)
			}
		})
	}
}
