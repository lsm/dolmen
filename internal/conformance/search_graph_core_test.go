package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/store"
)

func TestSearchGraphCoreCommittedRows(t *testing.T) {
	h := newHarness(t)
	eng, ok := h.st.(interface {
		SearchGraph(context.Context, string, []string, store.VectorQuery, int, graph.TraversalQuery, store.GraphAccess) (store.GraphSearchResult, error)
	})
	if testEngine(t) != "sqlite" {
		if ok {
			t.Fatal("graph search unexpectedly exposed by an unsupported adapter")
		}
		return
	}
	if !ok {
		t.Fatal("SQLite graph search adapter missing")
	}
	h.seedTable("acme", "nodes", []map[string]any{{"name": "v", "type": "vector", "dim": 2}, {"name": "label", "type": "string"}})
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "nodes", "records": []any{map[string]any{"v": []float64{1, 0}, "label": "keep"}, map[string]any{"v": []float64{0, 1}, "label": "drop"}}})
	out, err := eng.SearchGraph(t.Context(), "acme", []string{"nodes"}, store.VectorQuery{Vec: []float32{1, 0}, Filter: "label = ?", Args: []any{"keep"}}, 1, graph.TraversalQuery{Direction: "both", MaxHops: 4, MaxWork: 1000, Limits: graph.Limits{Nodes: 1000, Edges: 1000, Elapsed: time.Second}}, nil)
	if err != nil || len(out.Seeds) != 1 || out.Seeds[0].Score != 1 || len(out.Nodes) != 1 || out.Truncated {
		t.Fatalf("committed rows not searchable: %+v %v", out, err)
	}
}
