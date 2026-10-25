package conformance

import (
	"reflect"
	"testing"
)

func neighborsCall(t *testing.T, h *harness, transport string, id identity, body map[string]any) (map[string]any, string) {
	t.Helper()
	if transport == "http" {
		_, out := h.httpCallAs(id, "neighbors", body)
		if out["ok"] == true {
			return out["data"].(map[string]any), ""
		}
		return nil, errCode(out)
	}
	out := h.mcpCallAs(id, "neighbors", body)
	if out.proto != nil {
		t.Fatalf("neighbors protocol error: %+v", out.proto)
	}
	if out.isError() {
		code, _ := out.toolError()["code"].(string)
		return nil, code
	}
	return out.structured(), ""
}

func TestNeighborsTransports(t *testing.T) {
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarness(t)
			relationSetup(t, h)
			h.mustHTTP("relate", map[string]any{"namespace": "acme", "table": "links", "source": 1, "target": 2, "type": "knows"})
			body := map[string]any{"namespace": "acme", "node": map[string]any{"table": "nodes", "id": 1}}
			full, code := neighborsCall(t, h, transport, identity{}, body)
			if code != "" || full["stop_reason"] != "complete" || len(full["edges"].([]any)) != 1 || len(full["nodes"].([]any)) != 2 {
				t.Fatalf("neighbors: %v %s", full, code)
			}
			body["node"] = map[string]any{"table": "nodes", "id": 2}
			body["direction"] = "in"
			reverse, code := neighborsCall(t, h, transport, identity{}, body)
			if code != "" || !reflect.DeepEqual(full, reverse) {
				t.Fatalf("reverse: %v %s", reverse, code)
			}
			body["max_nodes"] = 1
			bounded, code := neighborsCall(t, h, transport, identity{}, body)
			if code != "" || bounded["truncated"] != true || bounded["stop_reason"] != "nodes" || len(bounded["edges"].([]any)) != 0 {
				t.Fatalf("bounds: %v %s", bounded, code)
			}
			delete(body, "max_nodes")
			body["relation_types"] = []string{"other"}
			filtered, code := neighborsCall(t, h, transport, identity{}, body)
			if code != "" || len(filtered["edges"].([]any)) != 0 || filtered["truncated"] != false {
				t.Fatalf("types: %v %s", filtered, code)
			}
			delete(body, "relation_types")
			for field, value := range map[string]any{"max_nodes": 0, "max_edges": 1001, "max_elapsed_ms": 30001, "direction": "sideways", "node": map[string]any{"table": "nodes", "id": 1.5}} {
				trial := map[string]any{"namespace": "acme", "node": map[string]any{"table": "nodes", "id": 1}}
				trial[field] = value
				if _, code := neighborsCall(t, h, transport, identity{}, trial); code != "invalid_request" {
					t.Fatalf("%s: %s", field, code)
				}
			}
			body["node"] = map[string]any{"table": "nodes", "id": 9007199254740993}
			delete(body, "direction")
			absent, code := neighborsCall(t, h, transport, identity{}, body)
			if code != "" || len(absent["nodes"].([]any)) != 0 {
				t.Fatalf("exact large id: %v %s", absent, code)
			}
		})
	}
}

func TestNeighborsGrantAndOwnerVisibility(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarnessMode(t, authAdminKey)
			ids := map[string]identity{}
			for _, p := range []string{"alice", "bob", "outsider"} {
				_, key := mintKey(t, h, p, p)
				ids[p] = identity{bearer: key}
			}
			h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
			h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "nodes", "row_access": "own", "accept_references": true, "fields": []any{map[string]any{"name": "label", "type": "string"}}})
			h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "links", "row_access": "own", "fields": []any{map[string]any{"name": "src", "type": "number", "required": true}, map[string]any{"name": "dst", "type": "number", "required": true}, map[string]any{"name": "kind", "type": "string", "required": true}}, "constraints": []any{map[string]any{"name": "src_ref", "kind": "foreign_key", "field": "src", "references": map[string]any{"table": "nodes", "field": "id"}}, map[string]any{"name": "dst_ref", "kind": "foreign_key", "field": "dst", "references": map[string]any{"table": "nodes", "field": "id"}}}, "relation": map[string]any{"source": "src", "target": "dst", "type": "kind"}})
			for _, principal := range []string{"alice", "bob"} {
				grantTo(t, h, "principal", principal, "acme", "", "read", "create")
			}
			for _, principal := range []string{"alice", "bob"} {
				h.mustHTTPAs(t, ids[principal], "insert", map[string]any{"namespace": "acme", "table": "nodes", "records": []any{map[string]any{"label": principal}}})
			}
			for _, e := range []struct {
				who string
				dst int
			}{{"alice", 1}, {"bob", 2}} {
				h.mustHTTPAs(t, ids[e.who], "relate", map[string]any{"namespace": "acme", "table": "links", "source": e.dst, "target": e.dst, "type": "knows"})
			}
			h.mustHTTP("revoke", map[string]any{"subject": map[string]any{"type": "principal", "id": "alice"}, "object": map[string]any{"namespace": "acme"}, "verbs": []string{"read", "create"}})
			body := map[string]any{"namespace": "acme", "node": map[string]any{"table": "nodes", "id": 1}}
			grantTo(t, h, "principal", "alice", "acme", "nodes", "update")
			out, code := neighborsCall(t, h, transport, ids["alice"], body)
			if code != "" || len(out["nodes"].([]any)) != 1 || len(out["edges"].([]any)) != 0 {
				t.Fatalf("denied edge table: %v %s", out, code)
			}
			grantTo(t, h, "principal", "alice", "acme", "links", "update")
			out, code = neighborsCall(t, h, transport, ids["alice"], body)
			if code != "" || len(out["edges"].([]any)) != 1 || len(out["nodes"].([]any)) != 1 {
				t.Fatalf("owner scopes: %v %s", out, code)
			}
			body["node"] = map[string]any{"table": "nodes", "id": 2}
			out, code = neighborsCall(t, h, transport, ids["alice"], body)
			if code != "" || len(out["nodes"].([]any)) != 0 || len(out["edges"].([]any)) != 0 {
				t.Fatalf("hidden seed: %v %s", out, code)
			}
			grantTo(t, h, "principal", "alice", "acme", "nodes", "read")
			out, code = neighborsCall(t, h, transport, ids["alice"], body)
			if code != "" || len(out["nodes"].([]any)) != 1 || len(out["edges"].([]any)) != 0 {
				t.Fatalf("visible seed but hidden edge owner: %v %s", out, code)
			}
			grantTo(t, h, "principal", "alice", "acme", "links", "read")
			out, code = neighborsCall(t, h, transport, ids["alice"], body)
			if code != "" || len(out["edges"].([]any)) != 1 {
				t.Fatalf("table-wide read: %v %s", out, code)
			}
			if _, code = neighborsCall(t, h, transport, ids["outsider"], body); code != "not_found" {
				t.Fatalf("ungranted namespace: %s", code)
			}
		})
	}
}
