package store

import (
	"context"
	"slices"
	"testing"

	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/graph/graphtest"
	"github.com/lsm/dolmen/internal/schema"
)

type sqliteGraphBackend struct{ s *Store }

func (b sqliteGraphBackend) Namespace() string { return "ns" }

func (b sqliteGraphBackend) Index(hidden ...string) graph.Index {
	return b.s.GraphIndex(func(_ context.Context, sc *schema.TableSchema) (*RowScope, error) {
		if slices.Contains(hidden, sc.Name) {
			return &RowScope{Empty: true}, nil
		}
		return nil, nil
	})
}

func (b sqliteGraphBackend) Insert(t *testing.T, table string, rows ...map[string]any) []int64 {
	t.Helper()
	res, err := b.s.Insert(t.Context(), "ns", table, rows, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Ids
}

func (b sqliteGraphBackend) Update(t *testing.T, table string, id int64, values map[string]any) {
	t.Helper()
	if _, err := b.s.Update(t.Context(), "ns", table, "id = ?", []any{id}, values, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
}

func (b sqliteGraphBackend) Delete(t *testing.T, table string, id int64) {
	t.Helper()
	if _, err := b.s.Delete(t.Context(), "ns", table, "id = ?", []any{id}, DeleteOpts{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
}

func (b sqliteGraphBackend) Drop(t *testing.T, table string) {
	t.Helper()
	if err := b.s.DropTable(t.Context(), "ns", table, Incarnation{}); err != nil {
		t.Fatal(err)
	}
}

func TestGraphSQLiteIndexSuite(t *testing.T) {
	graphtest.Run(t, func(t *testing.T) graphtest.Backend {
		s := relationStore(t)
		if _, err := s.CreateTable(t.Context(), "ns", "loops", relationFields(), relationOpts("alpha", "alpha"), [16]byte{}); err != nil {
			t.Fatal(err)
		}
		return sqliteGraphBackend{s}
	})
}
