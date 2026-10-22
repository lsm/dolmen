package conformance

import (
	"net/http"
	"testing"
)

func wantInvalidReq(t *testing.T, h *harness, what, op string, body map[string]any) {
	t.Helper()
	status, out := h.httpCall(op, body)
	errEnv, _ := out["error"].(map[string]any)
	code, _ := errEnv["code"].(string)
	if status != http.StatusBadRequest || code != "invalid_request" {
		t.Fatalf("%s: status %d code %q, want 400 invalid_request: %v", what, status, code, out)
	}
}

func TestSearchVectorRejectsZeroVectorAndBlankText(t *testing.T) {
	h := newHarness(t)
	h.seedTable("sv", "t", []map[string]any{
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "v", "type": "vector", "dim": 3},
	})
	h.mustHTTP("insert", map[string]any{"namespace": "sv", "table": "t", "records": []map[string]any{
		{"body": "hello world", "v": []any{1, 0, 0}},
	}})
	wantInvalidReq(t, h, "zero vector", "search_vector", map[string]any{"namespace": "sv", "table": "t", "column": "v", "vector": []any{0, 0, 0}})
	wantInvalidReq(t, h, "whitespace-only text", "search_vector", map[string]any{"namespace": "sv", "table": "t", "text": "   "})
}
