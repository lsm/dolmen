package conformance

import (
	"net/http"
	"testing"
)

// wantDescribeServerOff is describe_server's exact response body on the
// conformance harness under auth:off — the fake provider's fixed status
// (provider "conformance", model "fake-model", identity
// "conformance|fake|v1"), keys in the JSON encoder's sorted order, trailing
// newline included. Spec §8.1 pins the off-mode contract byte for byte:
// additions are permitted only where they cannot change these bytes, and
// describe_server's auth extension is auth:on-only (§8.2), so under off
// these bytes are frozen.
const wantDescribeServerOff = `{"data":{"embedding":{"identity":"conformance|fake|v1","model":"fake-model","provider":"conformance","usable":true}},"ok":true}
`

// assertDescribeServerShape is the assert-on-boot check for a harness: the
// booted server must answer describe_server with exactly the v0.2.0 bytes.
// When later modes arrive with their own boot expectations (7b), authOff
// keeps this one unchanged.
func assertDescribeServerShape(t *testing.T, h *harness) {
	t.Helper()
	res, body := h.httpCallRaw("describe_server", "{}", "application/json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("describe_server status %d, want 200: %s", res.StatusCode, body)
	}
	if body != wantDescribeServerOff {
		t.Fatalf("describe_server response bytes changed under %q mode:\ngot:  %q\nwant: %q", h.mode.name, body, wantDescribeServerOff)
	}
}

// TestHarnessModeOffBootsV02Server pins the mode boot path itself: a server
// booted through newHarnessMode at authOff is the v0.2.0 server — the same
// path newHarness routes the whole existing suite through — answering
// describe_server with the unchanged bytes before and after traffic, and
// serving a write/read round trip in between.
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

	// The pinned bytes hold after traffic too: no auth surface appears lazily.
	assertDescribeServerShape(t, h)
}

// TestHarnessModeOffIgnoresIdentity drives the identity-carrying helpers
// across stateless reads, writes, and error paths on both transports: under
// auth:off an identity-carrying call is the anonymous call — identical for
// the stateless ops, and (asserted inside the helpers) free of the auth
// statuses and of any identity string surfacing in the response.
func TestHarnessModeOffIgnoresIdentity(t *testing.T) {
	h := newHarnessMode(t, authOff)
	// A full identity: header pair plus bearer, every source asserted at once.
	id := identity{
		principal: "conformance-alice",
		groups:    []string{"conformance-team", "conformance-readers"},
		bearer:    "conformance-secret-bearer",
	}

	// Stateless ops: identity-carrying equals anonymous, response for
	// response (describe_server's bytes are separately pinned to the
	// v0.2.0 body, so the equality is byte-level there).
	for _, op := range []string{"describe_server", "list_namespaces"} {
		status, out := h.httpCallAs(id, op, map[string]any{})
		if status != http.StatusOK || out["ok"] != true {
			t.Fatalf("%s as %q failed: status %d %v", op, id.principal, status, out)
		}
		_, anon := h.httpCall(op, map[string]any{})
		assertJSONEqual(t, op+" as "+id.principal, out, anon)
	}

	// The same stateless equality over MCP, pinned to the known body.
	res := h.mcpCallAs(id, "describe_server", map[string]any{})
	if res.status != http.StatusOK || res.proto != nil || res.isError() {
		t.Fatalf("describe_server as %q over MCP failed: %+v", id.principal, res)
	}
	assertJSONEqual(t, "describe_server as "+id.principal+" over MCP", res.structured(), map[string]any{
		"embedding": map[string]any{
			"provider": "conformance",
			"usable":   true,
			"model":    "fake-model",
			"identity": "conformance|fake|v1",
		},
	})

	// Writes and reads as the principal, over both transports.
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

	// Error paths are identity-blind too: the same not_found as the
	// anonymous contract, with no principal in the envelope.
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
