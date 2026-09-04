package conformance

import (
	"net/http"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

// TestEngineSeam runs the op layer over the second, non-SQLite engine and
// asserts the store.Engine seam holds (dolmen#76): the registry ops behave
// through both transports exactly as they do over the SQLite adapter, and
// the ops the engine does not implement fail through the standard error
// envelope instead of escaping it. If this test breaks because an api
// change reached around the interface, the seam leaked.
func TestEngineSeam(t *testing.T) {
	h := newEngineHarness(t, store.OpenMem(), &fakeProvider{})

	// Namespace lifecycle over HTTP, with the same envelopes the SQLite
	// engine produces.
	h.mustHTTP("create_namespace", map[string]any{"namespace": "demo"})
	assertJSONEqual(t, "list_namespaces",
		h.mustHTTP("list_namespaces", map[string]any{}),
		map[string]any{"namespaces": []any{"demo"}})
	status, out := h.httpCall("create_namespace", map[string]any{"namespace": "demo"})
	if status != http.StatusBadRequest {
		t.Fatalf("duplicate create_namespace: status %d %v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "invalid_request" {
		t.Fatalf("duplicate create_namespace: code %v", errObj["code"])
	}
	wantMessage(t, "duplicate create_namespace", errObj["message"].(string),
		`^namespace demo already exists$`)

	// Table registry: create, list, describe — through both transports.
	h.seedTable("demo", "notes", []map[string]any{
		{"name": "title", "type": "string", "required": true},
		{"name": "body", "type": "text"},
	})
	assertJSONEqual(t, "list_tables",
		h.mustHTTP("list_tables", map[string]any{"namespace": "demo"}),
		map[string]any{"tables": []any{"notes"}})
	desc := h.mustMCP("describe_table", map[string]any{"namespace": "demo", "table": "notes"})
	table, _ := desc["table"].(map[string]any)
	if table["version"] != float64(1) {
		t.Fatalf("describe_table over the seam: version %v, want 1", table["version"])
	}
	if desc["row_count"] != float64(0) {
		t.Fatalf("describe_table over the seam: row_count %v, want 0", desc["row_count"])
	}

	// Ops the engine does not implement must refuse through the standard
	// envelope, naming the seam — on both transports.
	status, out = h.httpCall("insert", map[string]any{
		"namespace": "demo", "table": "notes",
		"records": []map[string]any{{"title": "first"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("insert over the seam: status %d %v", status, out)
	}
	errObj, _ = out["error"].(map[string]any)
	if errObj["code"] != "invalid_request" {
		t.Fatalf("insert over the seam: code %v", errObj["code"])
	}
	wantMessage(t, "insert over the seam", errObj["message"].(string),
		`^insert is not implemented by this engine \(MemEngine exists to prove the store\.Engine seam; see dolmen#76\)$`)
	res := h.mcpCall("query", map[string]any{"namespace": "demo", "sql": "SELECT * FROM notes"})
	if !res.isError() {
		t.Fatalf("query over the seam: expected a tool error, got %+v", res)
	}
	if msg := res.toolError()["message"].(string); msg != "query is not implemented by this engine (MemEngine exists to prove the store.Engine seam; see dolmen#76)" {
		t.Fatalf("query over the seam: message %q", msg)
	}

	// Drop paths classify not-found exactly like the SQLite engine.
	h.mustHTTP("drop_table", map[string]any{"namespace": "demo", "table": "notes", "confirm": "notes"})
	status, out = h.httpCall("describe_table", map[string]any{"namespace": "demo", "table": "notes"})
	if status != http.StatusNotFound {
		t.Fatalf("describe after drop: status %d %v", status, out)
	}
	errObj, _ = out["error"].(map[string]any)
	if errObj["code"] != "not_found" {
		t.Fatalf("describe after drop: code %v", errObj["code"])
	}
	wantMessage(t, "describe after drop", errObj["message"].(string),
		`^table demo\.notes$`)
}
