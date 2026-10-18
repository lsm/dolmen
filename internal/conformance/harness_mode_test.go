package conformance

import (
	"encoding/json"
	"net/http"
	"testing"
)

func wantDescribeServerOff(h *harness) string {
	store := "local"
	if h.fileStore.Remote() {
		store = "s3"
	}
	return `{"data":{"embedding":{"identity":"conformance|fake|v1","model":"fake-model","model_state":"download_on_first_use","provider":"conformance","usable":true},"files":{"enabled":true,"fetch_hosts":[],"max_file_bytes":1073741824,"max_namespace_bytes":0,"max_request_bytes":2147483648,"store":"` + store + `"}},"ok":true}
`
}

func assertDescribeServerShape(t *testing.T, h *harness) {
	t.Helper()
	res, body := h.httpCallRaw("describe_server", "{}", "application/json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("describe_server status %d, want 200: %s", res.StatusCode, body)
	}
	if want := wantDescribeServerOff(h); body != want {
		t.Fatalf("describe_server response bytes changed under %q mode:\ngot:  %q\nwant: %q", h.mode.name, body, want)
	}
}

func TestHarnessModeOffBootsV02Server(t *testing.T) {
	h := newHarnessMode(t, authOff)
	assertDescribeServerShape(t, h)

	ns := "modeoff"
	h.mustHTTP("create_namespace", map[string]any{"namespace": ns})
	h.seedTable(ns, "docs", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": ns, "table": "docs",
		"records": []map[string]any{{"title": "hello"}},
	})
	data := h.mustHTTP("query", map[string]any{"namespace": ns, "sql": "SELECT id, title FROM docs"})
	if got := int64val(t, "row_count", data["row_count"]); got != 1 {
		t.Fatalf("row_count %d, want 1: %v", got, data)
	}
	if rows, _ := data["rows"].([]any); len(rows) != 1 {
		t.Fatalf("query returned %d rows, want 1: %v", len(rows), data)
	}

	assertDescribeServerShape(t, h)
}

func TestHarnessModeOffIgnoresIdentity(t *testing.T) {
	h := newHarnessMode(t, authOff)

	id := identity{
		principal: "conformance-alice",
		groups:    []string{"conformance-team", "conformance-readers"},
		bearer:    "conformance-secret-bearer",
	}

	for _, op := range []string{"describe_server", "list_namespaces"} {
		status, out := h.httpCallAs(id, op, map[string]any{})
		if status != http.StatusOK || out["ok"] != true {
			t.Fatalf("%s as %q failed: status %d %v", op, id.principal, status, out)
		}
		_, anon := h.httpCall(op, map[string]any{})
		assertJSONEqual(t, op+" as "+id.principal, out, anon)
	}

	res := h.mcpCallAs(id, "describe_server", map[string]any{})
	if res.status != http.StatusOK || res.proto != nil || res.isError() {
		t.Fatalf("describe_server as %q over MCP failed: %+v", id.principal, res)
	}
	var want struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(wantDescribeServerOff(h)), &want); err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, "describe_server as "+id.principal+" over MCP", res.structured(), want.Data)

	ns := "modeid"
	if status, out := h.httpCallAs(id, "create_namespace", map[string]any{"namespace": ns}); status != http.StatusOK {
		t.Fatalf("create_namespace as %q failed: status %d %v", id.principal, status, out)
	}
	if status, out := h.httpCallAs(id, "create_table", map[string]any{
		"namespace": ns,
		"table":     "docs",
		"fields":    []map[string]any{{"name": "title", "type": "string"}},
	}); status != http.StatusOK {
		t.Fatalf("create_table as %q failed: status %d %v", id.principal, status, out)
	}
	res = h.mcpCallAs(id, "insert", map[string]any{
		"namespace": ns, "table": "docs",
		"records": []map[string]any{{"title": "hello"}},
	})
	if res.status != http.StatusOK || res.proto != nil || res.isError() {
		t.Fatalf("insert as %q over MCP failed: %+v", id.principal, res)
	}
	res = h.mcpCallAs(id, "query", map[string]any{"namespace": ns, "sql": "SELECT id, title FROM docs"})
	if res.status != http.StatusOK || res.proto != nil || res.isError() {
		t.Fatalf("query as %q over MCP failed: %+v", id.principal, res)
	}
	if sc := res.structured(); sc == nil {
		t.Fatalf("query as %q over MCP returned no structuredContent: %+v", id.principal, res)
	} else if got := int64val(t, "row_count", sc["row_count"]); got != 1 {
		t.Fatalf("row_count %d, want 1: %v", got, sc)
	}

	status, out := h.httpCallAs(id, "describe_table", map[string]any{"namespace": ns, "table": "nope"})
	if status != http.StatusNotFound {
		t.Fatalf("describe_table missing as %q: status %d, want 404: %v", id.principal, status, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if errEnv == nil || errEnv["code"] != "not_found" {
		t.Fatalf("describe_table missing as %q: error envelope %v", id.principal, out)
	}
	res = h.mcpCallAs(id, "describe_table", map[string]any{"namespace": ns, "table": "nope"})
	if !res.isError() {
		t.Fatalf("describe_table missing as %q over MCP did not fail: %+v", id.principal, res)
	}
	if code := res.toolError()["code"]; code != "not_found" {
		t.Fatalf("describe_table missing as %q over MCP: error code %v, want not_found", id.principal, code)
	}
}
