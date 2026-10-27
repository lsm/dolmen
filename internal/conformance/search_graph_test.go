package conformance

import "testing"

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
			for _, bad := range []map[string]any{{"direction": "invalid"}, {"max_hops": 33}, {"max_elapsed_ms": 0}, {"max_work": 0}} {
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
