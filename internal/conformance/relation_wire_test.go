package conformance

import (
	"net/http"
	"strings"
	"testing"
)

func relationTableBody(table string) map[string]any {
	return map[string]any{"namespace": "app", "table": table,
		"fields": []map[string]any{
			{"name": "src", "type": "number", "required": true},
			{"name": "dst", "type": "number", "required": true},
			{"name": "kind", "type": "string", "required": true},
			{"name": "weight", "type": "number"},
		},
		"constraints": []map[string]any{
			{"name": "src_ref", "kind": "foreign_key", "field": "src", "references": map[string]any{"table": "nodes", "field": "id"}},
			{"name": "dst_ref", "kind": "foreign_key", "field": "dst", "references": map[string]any{"table": "nodes", "field": "id"}},
		},
		"relation": map[string]any{"source": "src", "target": "dst", "type": "kind"}}
}

func TestRelationTablesAreDeclaredOnBothTransports(t *testing.T) {
	h := newHarness(t)
	declaringEngine(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "nodes", "accept_references": true, "fields": []map[string]any{{"name": "label", "type": "string"}}})
	created := map[string]map[string]any{
		"http": h.mustHTTP("create_table", relationTableBody("links_http")),
		"mcp":  h.mustMCP("create_table", relationTableBody("links_mcp")),
	}
	for transport, out := range created {
		rel, _ := out["table"].(map[string]any)["relation"].(map[string]any)
		if rel["source"] != "src" || rel["target"] != "dst" || rel["type"] != "kind" {
			t.Fatalf("%s create_table lost the relation: %v", transport, out)
		}
		table := "links_" + transport
		described := h.mustMCP("describe_table", map[string]any{"namespace": "app", "table": table})
		if rel, _ := described["table"].(map[string]any)["relation"].(map[string]any); rel["type"] != "kind" {
			t.Fatalf("%s describe_table lost the relation: %v", transport, described)
		}
		code, res := h.httpCall("insert", map[string]any{"namespace": "app", "table": table, "records": []map[string]any{{"src": 1, "dst": 1, "kind": "k"}}})
		if code != http.StatusBadRequest || errCode(res) != "conflict" || !strings.Contains(errMessage(res), "src_ref") {
			t.Fatalf("%s: an edge to a missing row: %d %v", transport, code, res)
		}
	}
	bad := relationTableBody("links_bad")
	bad["relation"] = map[string]any{"source": "src", "target": "dst", "type": "weight"}
	if code, out := h.httpCall("create_table", bad); code != http.StatusBadRequest || errCode(out) != "invalid_request" || !strings.Contains(errMessage(out), "string") {
		t.Fatalf("a numeric type field: %d %v", code, out)
	}
	if env := h.mcpCall("create_table", bad).toolError(); env["code"] != "invalid_request" {
		t.Fatalf("mcp: a numeric type field: %v", env)
	}
}

func TestDeclarationsAreRefusedWhileAuthIsOn(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	plain := []map[string]any{{"name": "label", "type": "string"}}
	for label, body := range map[string]map[string]any{
		"accept_references": {"namespace": "app", "table": "nodes", "accept_references": true, "fields": plain},
		"constraints":       {"namespace": "app", "table": "nodes", "fields": plain, "constraints": []map[string]any{{"name": "label_key", "kind": "unique", "fields": []string{"label"}}}},
		"relation":          relationTableBody("links"),
	} {
		code, out := h.httpCall("create_table", body)
		if code != http.StatusBadRequest || !strings.Contains(errMessage(out), "-auth on") {
			t.Fatalf("%s under -auth on: %d %v", label, code, out)
		}
		if env := h.mcpCall("create_table", body).toolError(); env == nil || !strings.Contains(env["message"].(string), "-auth on") {
			t.Fatalf("mcp %s under -auth on: %v", label, env)
		}
	}
}
