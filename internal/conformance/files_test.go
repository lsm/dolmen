package conformance

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func fileTableFields() []map[string]any {
	return []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "doc", "type": "file", "max_bytes": 1024, "media_types": []string{"text/*", "application/pdf"}},
	}
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func errMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

func TestFileFieldDeclaration(t *testing.T) {
	h := newHarness(t)
	if testEngine(t) != store.EngineSQLite {
		h.ensureNS("fil")
		code, body := h.httpCall("create_table", map[string]any{"namespace": "fil", "table": "docs", "fields": fileTableFields()})
		if code == http.StatusOK || !strings.Contains(errMessage(body), "file fields are not supported on the") {
			t.Fatalf("engine %s must refuse a file field with an explanation: %d %v", testEngine(t), code, body)
		}
		return
	}
	h.seedTable("fil", "docs", fileTableFields())
	for _, desc := range []map[string]any{
		h.mustHTTP("describe_table", map[string]any{"namespace": "fil", "table": "docs"}),
		h.mustMCP("describe_table", map[string]any{"namespace": "fil", "table": "docs"}),
	} {
		f := desc["table"].(map[string]any)["fields"].([]any)[1].(map[string]any)
		if f["type"] != "file" || f["max_bytes"] != float64(1024) || len(f["media_types"].([]any)) != 2 {
			t.Fatalf("describe_table must report the file field and its constraints, got %v", f)
		}
	}
	for _, change := range []map[string]any{
		{"op": "set_fulltext", "name": "doc", "value": true},
		{"op": "set_vectorize", "name": "doc", "value": true},
		{"op": "set_enum", "name": "doc", "enum": []string{"a"}},
		{"op": "add_field", "field": map[string]any{"name": "att", "type": "file"}, "default": "x"},
	} {
		code, body := h.httpCall("migrate", map[string]any{"namespace": "fil", "table": "docs", "changes": []any{change}})
		if code != http.StatusBadRequest || !strings.Contains(errMessage(body), "not allowed on file fields") {
			t.Errorf("migrate %v: %d %v", change["op"], code, body)
		}
	}
	code, body := h.httpCall("upsert_by_key", map[string]any{"namespace": "fil", "table": "docs", "on": []string{"doc"}, "records": []any{map[string]any{"title": "x"}}})
	if code != http.StatusBadRequest || errCode(body) != "invalid_request" {
		t.Fatalf("a file field cannot be a natural key: %d %v", code, body)
	}
	h.mustHTTP("migrate", map[string]any{"namespace": "fil", "table": "docs", "expected_version": 1, "changes": []any{
		map[string]any{"op": "add_field", "field": map[string]any{"name": "scan", "type": "file", "max_bytes": 2048, "media_types": []string{"application/pdf"}}},
	}})
	h.mustHTTP("list_migrations", map[string]any{"namespace": "fil", "table": "docs"})
	h.mustMCP("list_migrations", map[string]any{"namespace": "fil", "table": "docs"})
}

func TestAFileValueWithoutItsBytesTeachesMultipart(t *testing.T) {
	if testEngine(t) != store.EngineSQLite {
		t.Skip("file fields are SQLite-only for now")
	}
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	rec := map[string]any{"namespace": "fil", "table": "docs", "records": []any{map[string]any{"doc": map[string]any{"part": "notes"}}}}
	code, body := h.httpCall("insert", rec)
	if code != http.StatusBadRequest || errCode(body) != "invalid_request" || !strings.Contains(errMessage(body), "curl -F") {
		t.Fatalf("http: %d %v", code, body)
	}
	env := h.mcpCall("insert", rec).toolError()
	if msg, _ := env["message"].(string); !strings.Contains(msg, "curl -F") {
		t.Fatalf("mcp: %v", env)
	}
}
