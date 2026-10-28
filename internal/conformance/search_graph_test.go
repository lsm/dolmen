package conformance

import (
	"context"
	"errors"
	"github.com/lsm/dolmen"
	"math"
	"reflect"
	"testing"
)

func TestSearchGraph(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarness(t)
			relationSetup(t, h)
			h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "seeds", "accept_references": true, "fields": []any{map[string]any{"name": "v", "type": "vector", "dim": 2}, map[string]any{"name": "label", "type": "string"}}})
			h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "seeds", "records": []any{map[string]any{"v": []float64{1, 0}, "label": "keep"}, map[string]any{"v": []float64{0, 1}, "label": "drop"}}})
			body := map[string]any{"namespace": "acme", "tables": []string{"seeds"}, "vector": []float64{1, 0}, "top_k": 1, "filter": "label = ?", "args": []any{"keep"}, "expansion": map[string]any{"max_hops": 2, "relation_types": []string{"knows"}}}
			out, code := traversalCall(t, h, transport, "search_graph", identity{}, body)
			if code != "" {
				t.Fatalf("search_graph: %s", code)
			}
			seeds := out["seeds"].([]any)
			if len(seeds) != 1 || seeds[0].(map[string]any)["score"] != float64(1) || len(out["nodes"].([]any)) != 1 || out["truncated"] != false {
				t.Fatalf("result: %+v", out)
			}
			body["filter"] = "label = 'absent'"
			body["args"] = []any{}
			out, code = traversalCall(t, h, transport, "search_graph", identity{}, body)
			if code != "" || len(out["seeds"].([]any)) != 0 || out["stop_reason"] != "complete" {
				t.Fatalf("empty: %+v %s", out, code)
			}
		})
	}
}

func TestSearchGraphExpansionAndBounds(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarness(t)
			relationSetup(t, h)
			h.mustHTTP("migrate", map[string]any{"namespace": "acme", "table": "nodes", "changes": []any{map[string]any{"op": "add_field", "field": map[string]any{"name": "v", "type": "vector", "dim": 2}}}})
			h.mustHTTP("update", map[string]any{"namespace": "acme", "table": "nodes", "filter": "id = 1", "set": map[string]any{"v": []float64{1, 0}}})
			h.mustHTTP("relate", map[string]any{"namespace": "acme", "table": "links", "source": 1, "target": 2, "type": "knows"})
			body := map[string]any{"namespace": "acme", "tables": []string{"nodes"}, "vector": []float64{1, 0}, "top_k": 1}
			out, code := traversalCall(t, h, transport, "search_graph", identity{}, body)
			if code != "" || len(out["nodes"].([]any)) != 2 || len(out["edges"].([]any)) != 1 {
				t.Fatalf("expansion: %+v %s", out, code)
			}
			for field, reason := range map[string]string{"max_nodes": "nodes", "max_hops": "hops"} {
				body["expansion"] = map[string]any{field: 1}
				out, code = traversalCall(t, h, transport, "search_graph", identity{}, body)
				if code != "" || out["truncated"] != true || out["stop_reason"] != reason {
					t.Fatalf("bound %s: %+v %s", field, out, code)
				}
			}
			body["expansion"] = map[string]any{"relation_types": []string{"other"}}
			out, code = traversalCall(t, h, transport, "search_graph", identity{}, body)
			if code != "" || len(out["nodes"].([]any)) != 1 || len(out["edges"].([]any)) != 0 {
				t.Fatalf("type: %+v %s", out, code)
			}
			for _, bad := range []map[string]any{{"direction": "invalid"}, {"max_hops": 33}, {"max_elapsed_ms": 0}, {"max_work": 0}, {"source": map[string]any{}}, {"seeds": []any{}}, {"namespace": ""}} {
				body["expansion"] = bad
				if _, code = traversalCall(t, h, transport, "search_graph", identity{}, body); code != "invalid_request" {
					t.Fatalf("invalid expansion %+v: %s", bad, code)
				}
			}
		})
	}
}

func TestSearchGraphTextAndAccess(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarnessMode(t, authAdminKey)
			_, key := mintKey(t, h, "reader", "reader")
			h.seedTable("acme", "docs", []map[string]any{{"name": "body", "type": "text", "vectorize": true}})
			h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "docs", "records": []any{map[string]any{"body": "synthetic seed"}}})
			body := map[string]any{"namespace": "acme", "tables": []string{"docs"}, "text": "synthetic seed"}
			id := identity{bearer: key}
			if _, code := traversalCall(t, h, transport, "search_graph", id, body); code != "forbidden" && code != "not_found" {
				t.Fatalf("hidden seed search: %s", code)
			}
			grantTo(t, h, "principal", "reader", "acme", "docs", "read")
			out, code := traversalCall(t, h, transport, "search_graph", id, body)
			if code != "" || len(out["seeds"].([]any)) != 1 || len(out["nodes"].([]any)) != 1 {
				t.Fatalf("visible semantic seed: %+v %s", out, code)
			}
			body["filter"] = "id = -1"
			body["expansion"] = map[string]any{"direction": "invalid"}
			if _, code := traversalCall(t, h, transport, "search_graph", id, body); code != "invalid_request" {
				t.Fatalf("empty search must validate expansion: %s", code)
			}
		})
	}
}

func TestSearchGraphFacade(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	h := newHarness(t)
	h.seedTable("acme", "docs", []map[string]any{{"name": "body", "type": "text", "vectorize": true}})
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "docs", "records": []any{map[string]any{"body": "synthetic seed"}}})
	s, err := dolmen.Open(h.dir, dolmen.WithEmbedding(h.emb))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, err := s.SearchGraph(t.Context(), "acme", []string{" Docs "}, dolmen.VectorQuery{Text: "synthetic seed"}, 0, dolmen.SearchOptions{}, dolmen.TraversalOptions{})
	if err != nil || len(out.Seeds) != 1 || out.Seeds[0].Ref != (dolmen.NodeRef{Table: "docs", ID: 1}) || len(out.Nodes) != 1 || out.Truncated {
		t.Fatalf("Go seed: %+v %v", out, err)
	}
	sub, err := s.Subgraph(t.Context(), "acme", []dolmen.NodeRef{out.Seeds[0].Ref}, dolmen.TraversalOptions{})
	if err != nil || len(sub.Nodes) != 1 {
		t.Fatalf("Go seed not reusable in graph calls: %+v %v", sub, err)
	}
	bad := math.NaN()
	if _, err = s.SearchGraph(t.Context(), "acme", []string{"docs"}, dolmen.VectorQuery{Text: "synthetic seed", MinScore: &bad}, 1, dolmen.SearchOptions{}, dolmen.TraversalOptions{}); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("non-finite min_score: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.SearchGraph(ctx, "acme", []string{"docs"}, dolmen.VectorQuery{Text: "synthetic seed"}, 1, dolmen.SearchOptions{}, dolmen.TraversalOptions{}); !errors.Is(err, dolmen.ErrCanceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestSearchGraphTransportsTopK(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	h := newHarness(t)
	h.seedTable("acme", "docs", []map[string]any{{"name": "v", "type": "vector", "dim": 2}})
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "docs", "records": []any{map[string]any{"v": []float64{1, 0}}, map[string]any{"v": []float64{0, 1}}}})
	for _, transport := range []string{"http", "mcp"} {
		body := map[string]any{"namespace": "acme", "tables": []string{"docs"}, "vector": []float64{1, 0}}
		out, code := traversalCall(t, h, transport, "search_graph", identity{}, body)
		if code != "" || len(out["seeds"].([]any)) != 2 {
			t.Fatalf("%s default: %+v %s", transport, out, code)
		}
		for _, k := range []int{0, -1, 201} {
			body["top_k"] = k
			if _, code = traversalCall(t, h, transport, "search_graph", identity{}, body); code != "invalid_request" {
				t.Fatalf("%s top_k=%d: %s", transport, k, code)
			}
		}
	}
}

func TestSearchGraphFacadeOptionsReuse(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	h := newHarness(t)
	h.seedTable("acme", "docs", []map[string]any{{"name": "v", "type": "vector", "dim": 2}})
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "docs", "records": []any{map[string]any{"v": []float64{1, 0}}, map[string]any{"v": []float64{0, 1}}}})
	s, err := dolmen.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	query := dolmen.VectorQuery{Vec: []float32{1, 0}}
	baseline, err := s.SearchGraph(t.Context(), "acme", []string{"docs"}, query, 0, dolmen.SearchOptions{}, dolmen.TraversalOptions{})
	if err != nil || len(baseline.Seeds) != 2 {
		t.Fatalf("Go topK default: %+v %v", baseline, err)
	}
	reused, err := s.SearchGraph(t.Context(), "acme", []string{"docs"}, query, 0, dolmen.SearchOptions{Limit: 1, Offset: 1, IncludeHidden: true, Reveal: []string{"v"}}, dolmen.TraversalOptions{})
	if err != nil || !reflect.DeepEqual(reused, baseline) {
		t.Fatalf("irrelevant shared options affected graph result: %+v %v", reused, err)
	}
	_, err = s.SearchGraph(t.Context(), "acme", []string{"docs"}, query, 0, dolmen.SearchOptions{Limit: 201}, dolmen.TraversalOptions{})
	if !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("common options validation: %v", err)
	}
}

func TestSearchGraphRequiresSQLite(t *testing.T) {
	if testEngine(t) == "sqlite" {
		t.Skip("other-engine refusal proof")
	}
	h := newHarness(t)
	for _, transport := range []string{"http", "mcp"} {
		_, code := traversalCall(t, h, transport, "search_graph", identity{}, map[string]any{"namespace": "missing", "tables": []string{"nodes"}, "vector": []float64{1, 0}})
		if code != "invalid_request" {
			t.Fatalf("%s unsupported adapter: %s", transport, code)
		}
	}
}

func TestSearchGraphSeedTableAuthorization(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarnessMode(t, authAdminKey)
			_, key := mintKey(t, h, "reader", "reader")
			h.seedTable("acme", "visible", []map[string]any{{"name": "v", "type": "vector", "dim": 2}})
			h.seedTable("acme", "hidden", []map[string]any{{"name": "v", "type": "vector", "dim": 3}})
			grantTo(t, h, "principal", "reader", "acme", "visible", "read")
			for _, table := range []string{"hidden", "absent"} {
				body := map[string]any{"namespace": "acme", "tables": []string{table}, "vector": []float64{1, 0}}
				_, code := traversalCall(t, h, transport, "search_graph", identity{bearer: key}, body)
				if code != "forbidden" {
					t.Fatalf("unreadable seed table %s leaked validation/existence: %s", table, code)
				}
			}
		})
	}
}
