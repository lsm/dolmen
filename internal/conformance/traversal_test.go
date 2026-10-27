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
