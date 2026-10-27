package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/schema"
)

func searchGraphFixture(t *testing.T) *Store {
	t.Helper()
	s := openRowAccessStore(t)
	for _, table := range []string{"alpha", "beta"} {
		_, err := s.CreateTable(t.Context(), "ns", table, []schema.Field{{Name: "label", Type: schema.String}, {Name: "v", Type: schema.Vector, Dim: 2}}, TableOpts{AcceptReferences: true}, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if err := relInsert(t, s, table, map[string]any{"label": "keep", "v": []float32{1, 0}}, map[string]any{"label": "drop", "v": []float32{0, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateTable(t.Context(), "ns", "links", relationFields(), relationOpts("alpha", "beta"), [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := relInsert(t, s, "links", map[string]any{"src": 1, "dst": 1, "kind": "knows"}); err != nil {
		t.Fatal(err)
	}
	return s
}
func searchExpansion() graph.TraversalQuery {
	return graph.TraversalQuery{Direction: "out", MaxHops: 4, MaxWork: 1000, Limits: graph.Limits{Nodes: 1000, Edges: 1000, Elapsed: time.Second}}
}
func TestSearchGraphCore(t *testing.T) {
	s := searchGraphFixture(t)
	q := VectorQuery{Vec: []float32{1, 0}, Filter: "label = ?", Args: []any{"keep"}}
	ex := searchExpansion()
	out, err := s.SearchGraph(t.Context(), "ns", []string{"beta", "alpha"}, q, 1, ex, nil)
	if err != nil || len(out.Seeds) != 1 || out.Seeds[0].Ref.Table != "alpha" || out.Seeds[0].Score != 1 || len(out.Nodes) != 2 || len(out.Edges) != 1 || out.Truncated {
		t.Fatalf("rank/expansion: %+v %v", out, err)
	}
	ex.RelationTypes = []string{"other"}
	out, err = s.SearchGraph(t.Context(), "ns", []string{"alpha"}, q, 1, ex, nil)
	if err != nil || len(out.Nodes) != 1 || len(out.Edges) != 0 || out.Truncated {
		t.Fatalf("type filter: %+v %v", out, err)
	}
	ex = searchExpansion()
	ex.Limits.Nodes = 1
	out, err = s.SearchGraph(t.Context(), "ns", []string{"alpha"}, q, 1, ex, nil)
	if err != nil || !out.Truncated || out.StopReason != "nodes" {
		t.Fatalf("bound: %+v %v", out, err)
	}
	q.Args = []any{"absent"}
	out, err = s.SearchGraph(t.Context(), "ns", []string{"alpha"}, q, 1, searchExpansion(), nil)
	if err != nil || len(out.Seeds) != 0 || len(out.Nodes) != 0 || out.Truncated {
		t.Fatalf("empty: %+v %v", out, err)
	}
	ex.Direction = "invalid"
	if _, err = s.SearchGraph(t.Context(), "ns", []string{"alpha"}, q, 1, ex, nil); !errors.Is(err, derr.ErrInvalidRequest) {
		t.Fatalf("empty validates bounds: %v", err)
	}
}
func TestSearchGraphSeedAndExpansionShareSnapshot(t *testing.T) {
	s := searchGraphFixture(t)
	calls := 0
	access := func(ctx context.Context, sc *schema.TableSchema) (*RowScope, error) {
		if sc.Name == "links" {
			calls++
			if calls == 2 {
				if _, err := s.Update(ctx, "ns", "alpha", "id = 1", nil, map[string]any{"v": []float32{0, 1}}, Embedder{}, nil, Incarnation{}); err != nil {
					return nil, err
				}
				return nil, relInsert(t, s, "links", map[string]any{"src": 1, "dst": 2, "kind": "later"})
			}
		}
		return nil, nil
	}
	out, err := s.SearchGraph(t.Context(), "ns", []string{"alpha"}, VectorQuery{Vec: []float32{1, 0}}, 1, searchExpansion(), access)
	if err != nil || len(out.Seeds) != 1 || out.Seeds[0].Score != 1 || len(out.Edges) != 1 || out.Edges[0].Target.ID != 1 {
		t.Fatalf("mixed snapshot: %+v %v", out, err)
	}
	out, err = s.SearchGraph(t.Context(), "ns", []string{"alpha"}, VectorQuery{Vec: []float32{1, 0}}, 1, searchExpansion(), nil)
	if err != nil || out.Seeds[0].Score != 0 || len(out.Edges) != 2 {
		t.Fatalf("fresh snapshot: %+v %v", out, err)
	}
}
func TestSearchGraphAccessAndElapsed(t *testing.T) {
	s := searchGraphFixture(t)
	for _, hidden := range []string{"alpha", "links", "beta"} {
		out, err := s.SearchGraph(t.Context(), "ns", []string{"alpha"}, VectorQuery{Vec: []float32{1, 0}}, 1, searchExpansion(), func(_ context.Context, sc *schema.TableSchema) (*RowScope, error) {
			if sc.Name == hidden {
				return &RowScope{Empty: true}, nil
			}
			return nil, nil
		})
		n := 1
		if hidden == "alpha" {
			n = 0
		}
		if err != nil || len(out.Seeds) != n || len(out.Nodes) != n || len(out.Edges) != 0 {
			t.Fatalf("hidden %s: %+v %v", hidden, out, err)
		}
	}
	ex := searchExpansion()
	ex.Limits.Elapsed = time.Millisecond
	out, err := s.SearchGraph(t.Context(), "ns", []string{"alpha"}, VectorQuery{Vec: []float32{1, 0}}, 1, ex, func(ctx context.Context, _ *schema.TableSchema) (*RowScope, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil || !out.Truncated || out.StopReason != "elapsed" {
		t.Fatalf("elapsed: %+v %v", out, err)
	}
}
