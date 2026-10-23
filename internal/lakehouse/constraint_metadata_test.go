package lakehouse

import (
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestConstraintMetadataLakehouseRefusesBeforeCatalogAccess(t *testing.T) {
	s := &Store{}
	for _, opts := range []store.TableOpts{{Constraints: []schema.Constraint{{}}}, {AcceptReferences: true}} {
		_, err := s.CreateTable(t.Context(), "app", "notes", []schema.Field{{Name: "slug", Type: schema.String}}, opts, [16]byte{})
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("unsupported options returned %v", err)
		}
	}
}

func TestConstraintMetadataLakehouseRefusesStoredDeclarations(t *testing.T) {
	for _, opts := range []store.TableOpts{
		{Constraints: []schema.Constraint{{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}},
		{AcceptReferences: true},
	} {
		t.Run("metadata", func(t *testing.T) {
			s := openStore(t, t.TempDir())
			ctx := t.Context()
			if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if err := s.withNamespace(ctx, "app", func(n *namespace) error {
				state, err := loadTable(ctx, n, "app", "notes")
				if err != nil {
					return err
				}
				state.schema.Constraints, state.schema.AcceptReferences = opts.Constraints, opts.AcceptReferences
				return s.publishSchema(ctx, n, state)
			}); err != nil {
				t.Fatal(err)
			}
			_, _, err := s.DescribeTable(ctx, "app", "notes", nil, store.Incarnation{})
			if !errors.Is(err, store.ErrCatalogTooNew) {
				t.Fatalf("describe ignored metadata: %v", err)
			}
			_, err = s.Insert(ctx, "app", "notes", []map[string]any{{"slug": "x"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
			if !errors.Is(err, store.ErrCatalogTooNew) {
				t.Fatalf("insert ignored metadata: %v", err)
			}
			if err := s.DropTable(ctx, "app", "notes", store.Incarnation{}); !errors.Is(err, store.ErrCatalogTooNew) {
				t.Fatalf("drop ignored metadata: %v", err)
			}
		})
	}
}
