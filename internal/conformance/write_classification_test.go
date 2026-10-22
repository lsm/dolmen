package conformance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestBatchClassifiesABadNamespaceAsInvalid(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	cases := []struct {
		name string
		body string
	}{
		{"missing namespace", `{"writes":[{"kind":"delete","table":"docs","filter":"1=0"}]}`},
		{"empty namespace", `{"namespace":"","writes":[{"kind":"delete","table":"docs","filter":"1=0"}]}`},
		{"malformed namespace", `{"namespace":"Bad NS","writes":[{"kind":"delete","table":"docs","filter":"1=0"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := h.httpCall("batch", json.RawMessage([]byte(tc.body)))
			errEnv, _ := out["error"].(map[string]any)
			code, _ := errEnv["code"].(string)
			if status != http.StatusBadRequest || code != "invalid_request" {
				t.Fatalf("%s: status %d code %q, want 400 invalid_request: %v", tc.name, status, code, out)
			}
			res := h.mcpCall("batch", json.RawMessage([]byte(tc.body)))
			if res.toolError() == nil {
				t.Fatalf("%s: MCP accepted what /v1 refused", tc.name)
			}
		})
	}
}

func TestWritesRejectANulByteInAStringValue(t *testing.T) {
	h := newHarness(t)
	h.seedTable("nul", "notes", []map[string]any{
		{"name": "body", "type": "text"},
		{"name": "token", "type": "secret"},
	})
	wantInvalid := func(what, op string, body map[string]any) {
		status, out := h.httpCall(op, body)
		errEnv, _ := out["error"].(map[string]any)
		code, _ := errEnv["code"].(string)
		msg, _ := errEnv["message"].(string)
		if status != http.StatusBadRequest || code != "invalid_request" {
			t.Fatalf("%s: status %d code %q, want 400 invalid_request: %v", what, status, code, out)
		}
		if !strings.Contains(msg, `"body"`) && !strings.Contains(msg, `"token"`) {
			t.Fatalf("%s: the refusal %q must name the field", what, msg)
		}
	}
	wantInvalid("insert text", "insert", map[string]any{"namespace": "nul", "table": "notes", "records": []map[string]any{{"body": "a \x00 b"}}})
	wantInvalid("insert secret", "insert", map[string]any{"namespace": "nul", "table": "notes", "records": []map[string]any{{"token": "a \x00 b"}}})

	h.mustHTTP("insert", map[string]any{"namespace": "nul", "table": "notes", "records": []map[string]any{{"body": "ok"}}})
	wantInvalid("update", "update", map[string]any{"namespace": "nul", "table": "notes", "filter": "body = ?", "args": []any{"ok"}, "set": map[string]any{"body": "x \x00 y"}})
	wantInvalid("batch", "batch", map[string]any{"namespace": "nul", "writes": []map[string]any{
		{"kind": "insert", "table": "notes", "records": []map[string]any{{"body": "z \x00 z"}}},
	}})

	data := h.mustHTTP("query", map[string]any{"namespace": "nul", "sql": "SELECT count(*) AS n FROM notes"})
	rows, _ := data["rows"].([]any)
	if n := int64val(t, "n", rows[0].(map[string]any)["n"]); n != 1 {
		t.Fatalf("a rejected NUL write changed the table: %d rows, want 1", n)
	}
}
