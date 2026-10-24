package conformance

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
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
	if testEngine(t) != store.EngineSQLite {
		t.Skip("relation tables are SQLite-only so far")
	}
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

func createTableInputProps(t *testing.T, h *harness) map[string]any {
	t.Helper()
	res := h.rpc(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools, _ := res.result["tools"].([]any)
	for _, tool := range tools {
		if m := tool.(map[string]any); m["name"] == "create_table" {
			return m["inputSchema"].(map[string]any)["properties"].(map[string]any)
		}
	}
	t.Fatal("tools/list has no create_table")
	return nil
}

func TestDeclarationsAreAdvertisedOnlyWhileAuthIsOff(t *testing.T) {
	for _, mode := range []harnessMode{authOff, authAdminKey} {
		props := createTableInputProps(t, newHarnessMode(t, mode))
		for _, name := range []string{"constraints", "accept_references", "relation"} {
			if _, ok := props[name]; ok != mode.off() {
				t.Fatalf("auth %s: create_table advertises %s = %v, want %v", mode.name, name, ok, mode.off())
			}
		}
	}
}

func TestUndeclaringEnginesNameTheEngineAsTheGap(t *testing.T) {
	if testEngine(t) != store.EngineLakehouse {
		t.Skip("only lakehouse still keeps no declarations")
	}
	h := newHarness(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	code, out := h.httpCall("create_table", map[string]any{"namespace": "app", "table": "nodes", "accept_references": true, "fields": []map[string]any{{"name": "label", "type": "string"}}})
	if code != http.StatusBadRequest || !strings.Contains(errMessage(out), "storage engine") {
		t.Fatalf("%s: %d %v, want a refusal naming the storage engine", testEngine(t), code, out)
	}
}
