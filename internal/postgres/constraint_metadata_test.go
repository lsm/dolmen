package postgres

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestConstraintMetadataPostgresRefusesBeforeDatabaseAccess(t *testing.T) {
	s := &Store{tr: newTracer(nil, nil)}
	for _, opts := range []store.TableOpts{{Constraints: []schema.Constraint{{}}}, {AcceptReferences: true}} {
		_, err := s.CreateTable(t.Context(), "app", "notes", []schema.Field{{Name: "slug", Type: schema.String}}, opts, [16]byte{})
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("unsupported options returned %v", err)
		}
	}
}

func TestConstraintMetadataPostgresDecodeGuard(t *testing.T) {
	for _, sc := range []schema.TableSchema{
		{Name: "notes", Version: 1},
		{Name: "notes", Version: 1, Constraints: []schema.Constraint{}},
		{Name: "notes", Version: 1, Constraints: []schema.Constraint{{}}},
		{Name: "notes", Version: 1, AcceptReferences: true},
	} {
		raw, err := json.Marshal(sc)
		if err != nil {
			t.Fatal(err)
		}
		var state tableState
		err = decodeTableState(&state, namespace{name: "app"}, "notes", string(raw), `{}`, 0)
		unsupported := len(sc.Constraints) > 0 || sc.AcceptReferences
		if unsupported && !errors.Is(err, store.ErrCatalogTooNew) || !unsupported && err != nil {
			t.Fatalf("metadata guard returned %v for %+v", err, sc)
		}
	}
}
