package conformance

import "testing"

func traversalCall(t *testing.T, h *harness, transport, op string, id identity, body map[string]any) (map[string]any, string) {
	t.Helper()
	if transport == "http" {
		_, out := h.httpCallAs(id, op, body)
		if out["ok"] == true {
			return out["data"].(map[string]any), ""
		}
		return nil, errCode(out)
	}
	out := h.mcpCallAs(id, op, body)
	if out.proto != nil {
		t.Fatalf("%s protocol error: %+v", op, out.proto)
	}
	if out.isError() {
		code, _ := out.toolError()["code"].(string)
		return nil, code
	}
	return out.structured(), ""
}

func TestGraphTraversalTransports(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarness(t)
			relationSetup(t, h)
			h.mustHTTP("relate", map[string]any{"namespace": "acme", "table": "links", "source": 1, "target": 2, "type": "knows"})
			ref := func(id int) map[string]any { return map[string]any{"table": "nodes", "id": id} }
			for _, op := range []string{"find_path", "subgraph"} {
				body := map[string]any{"namespace": "acme", "max_hops": 2}
				if op == "find_path" {
					body["source"], body["target"] = ref(1), ref(2)
				} else {
					body["seeds"] = []any{ref(1)}
				}
				out, code := traversalCall(t, h, transport, op, identity{}, body)
				if code != "" || out["truncated"] != false || out["stop_reason"] != "complete" || len(out["nodes"].([]any)) != 2 || len(out["edges"].([]any)) != 1 {
					t.Fatalf("%s: %+v %s", op, out, code)
				}
				body["max_nodes"] = 1
				out, code = traversalCall(t, h, transport, op, identity{}, body)
				if code != "" || out["truncated"] != true || out["stop_reason"] != "nodes" {
					t.Fatalf("%s node bound: %+v %s", op, out, code)
				}
			}
		})
	}
}

func TestGraphTraversalRequiresSQLite(t *testing.T) {
	if testEngine(t) == "sqlite" {
		t.Skip("other-engine refusal proof")
	}
	h := newHarness(t)
	for _, transport := range []string{"http", "mcp"} {
		for _, op := range []string{"find_path", "subgraph"} {
			body := map[string]any{"namespace": "missing", "seeds": []any{map[string]any{"table": "nodes", "id": 1}}}
			if op == "find_path" {
				delete(body, "seeds")
				body["source"], body["target"] = map[string]any{"table": "nodes", "id": 1}, map[string]any{"table": "nodes", "id": 2}
			}
			if _, code := traversalCall(t, h, transport, op, identity{}, body); code != "invalid_request" {
				t.Fatalf("%s/%s: %s", transport, op, code)
			}
		}
	}
}

func TestGraphTraversalWireBudgets(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarness(t)
			relationSetup(t, h)
			h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "nodes", "records": []any{map[string]any{"label": "n3"}}})
			for _, pair := range [][2]int{{1, 2}, {2, 3}} {
				h.mustHTTP("relate", map[string]any{"namespace": "acme", "table": "links", "source": pair[0], "target": pair[1], "type": "k"})
			}
			ref := func(id int) map[string]any { return map[string]any{"table": "nodes", "id": id} }
			for _, op := range []string{"find_path", "subgraph"} {
				base := map[string]any{"namespace": "acme", "direction": "out"}
				if op == "find_path" {
					base["source"], base["target"] = ref(1), ref(3)
				} else {
					base["seeds"] = []any{ref(1), ref(1)}
				}
				for field, reason := range map[string]string{"max_hops": "hops", "max_nodes": "nodes", "max_edges": "edges", "max_work": "work"} {
					body := map[string]any{}
					for k, v := range base {
						body[k] = v
					}
					body[field] = 1
					out, code := traversalCall(t, h, transport, op, identity{}, body)
					if code != "" || out["truncated"] != true || out["stop_reason"] != reason {
						t.Fatalf("%s/%s: %+v %s", op, field, out, code)
					}
				}
				for field, value := range map[string]any{"max_hops": 33, "max_nodes": 0, "max_edges": 1001, "max_work": 0, "max_elapsed_ms": 30001, "direction": "sideways"} {
					body := map[string]any{}
					for k, v := range base {
						body[k] = v
					}
					body[field] = value
					if _, code := traversalCall(t, h, transport, op, identity{}, body); code != "invalid_request" {
						t.Fatalf("%s/%s invalid bound: %s", op, field, code)
					}
				}
			}
		})
	}
}

func TestGraphTraversalCannotBridgeHiddenIntermediate(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarnessMode(t, authAdminKey)
			_, key := mintKey(t, h, "reader", "reader")
			id := identity{bearer: key}
			h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
			for _, table := range []string{"nodes", "hidden_nodes"} {
				h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": table, "accept_references": true, "fields": []any{map[string]any{"name": "label", "type": "string"}}})
				h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": table, "records": []any{map[string]any{"label": "first"}, map[string]any{"label": "second"}}})
			}
			for i, ends := range [][2]string{{"nodes", "hidden_nodes"}, {"hidden_nodes", "nodes"}} {
				table := []string{"links_a", "links_b"}[i]
				h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": table, "fields": []any{map[string]any{"name": "src", "type": "number", "required": true}, map[string]any{"name": "dst", "type": "number", "required": true}, map[string]any{"name": "kind", "type": "string", "required": true}}, "constraints": []any{map[string]any{"name": "src_ref", "kind": "foreign_key", "field": "src", "references": map[string]any{"table": ends[0], "field": "id"}}, map[string]any{"name": "dst_ref", "kind": "foreign_key", "field": "dst", "references": map[string]any{"table": ends[1], "field": "id"}}}, "relation": map[string]any{"source": "src", "target": "dst", "type": "kind"}})
				target := 1
				if i == 1 {
					target = 2
				}
				h.mustHTTP("relate", map[string]any{"namespace": "acme", "table": table, "source": 1, "target": target, "type": "k"})
				grantTo(t, h, "principal", "reader", "acme", table, "read")
			}
			grantTo(t, h, "principal", "reader", "acme", "nodes", "read")
			grantTo(t, h, "principal", "reader", "acme", "hidden_nodes", "update")
			for _, op := range []string{"find_path", "subgraph"} {
				body := map[string]any{"namespace": "acme", "direction": "out"}
				if op == "find_path" {
					body["source"], body["target"] = map[string]any{"table": "nodes", "id": 1}, map[string]any{"table": "nodes", "id": 2}
				} else {
					body["seeds"] = []any{map[string]any{"table": "nodes", "id": 1}}
				}
				out, code := traversalCall(t, h, transport, op, id, body)
				want := 0
				if op == "subgraph" {
					want = 1
				}
				if code != "" || out["truncated"] != false || len(out["nodes"].([]any)) != want || len(out["edges"].([]any)) != 0 {
					t.Fatalf("hidden bridge %s: %+v %s", op, out, code)
				}
				grantTo(t, h, "principal", "reader", "acme", "hidden_nodes", "read")
				out, code = traversalCall(t, h, transport, op, id, body)
				if code != "" || out["truncated"] != false || len(out["nodes"].([]any)) != 3 || len(out["edges"].([]any)) != 2 {
					t.Fatalf("visible bridge %s: %+v %s", op, out, code)
				}
				h.mustHTTP("revoke", map[string]any{"subject": map[string]any{"type": "principal", "id": "reader"}, "object": map[string]any{"namespace": "acme", "table": "hidden_nodes"}, "verbs": []string{"read"}})
			}
		})
	}
}
