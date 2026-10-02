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

func TestSearchLimitRejectsBelowOne(t *testing.T) {
	h := newHarness(t)
	h.seedTable("sl", "t", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "v", "type": "vector", "dim": 2},
	})
	h.mustHTTP("insert", map[string]any{"namespace": "sl", "table": "t", "records": []map[string]any{
		{"title": "alpha", "v": []any{1, 0}},
	}})
	for _, lim := range []any{0, -1} {
		wantInvalidReq(t, h, "fulltext limit", "search_fulltext", map[string]any{"namespace": "sl", "table": "t", "query": "alpha", "limit": lim})
		wantInvalidReq(t, h, "vector limit", "search_vector", map[string]any{"namespace": "sl", "table": "t", "column": "v", "vector": []any{1, 0}, "limit": lim})
	}
	ft := h.mustHTTP("search_fulltext", map[string]any{"namespace": "sl", "table": "t", "query": "alpha", "limit": 500})
	if int64val(t, "limit", ft["limit"]) != 200 {
		t.Fatalf("limit above 200 should clamp to 200: %v", ft["limit"])
	}
}
