package conformance

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	dolmen "github.com/lsm/dolmen"
)

func TestANulAnywhereInARequestIsRefusedNamingTheField(t *testing.T) {
	h := newHarness(t)
	h.seedTable("nul", "notes", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "prefs", "type": "json"},
	})
	h.mustHTTP("insert", map[string]any{"namespace": "nul", "table": "notes", "records": []map[string]any{{"title": "ok", "prefs": map[string]any{"k": "v"}}}})

	cases := []struct {
		what  string
		op    string
		body  map[string]any
		field string
	}{
		{"table name", "read_rows", map[string]any{"namespace": "nul", "table": "no\x00tes", "ids": []any{1}}, `"table"`},
		{"cursor", "changes_since", map[string]any{"namespace": "nul", "cursor": "ab\x00cd"}, `"cursor"`},
		{"idempotency key", "insert", map[string]any{"namespace": "nul", "table": "notes", "idempotency_key": "k\x00", "records": []map[string]any{{"title": "x"}}}, `"idempotency_key"`},
		{"query arg", "query", map[string]any{"namespace": "nul", "sql": "SELECT id FROM notes WHERE title = ?", "args": []any{"a\x00"}}, `"args"`},
		{"search text", "search_fulltext", map[string]any{"namespace": "nul", "table": "notes", "query": "o\x00k"}, `"query"`},
		{"json value", "insert", map[string]any{"namespace": "nul", "table": "notes", "records": []map[string]any{{"prefs": map[string]any{"k": "a\x00b"}}}}, `"k"`},
		{"json key", "insert", map[string]any{"namespace": "nul", "table": "notes", "records": []map[string]any{{"prefs": map[string]any{"a\x00b": 1}}}}, "field name"},
	}
	for _, c := range cases {
		status, out := h.httpCall(c.op, c.body)
		errEnv, _ := out["error"].(map[string]any)
		msg, _ := errEnv["message"].(string)
		if status != http.StatusBadRequest || errEnv["code"] != "invalid_request" || !strings.Contains(msg, c.field) || !strings.Contains(msg, "NUL") {
			t.Fatalf("%s: status %d %v, want 400 invalid_request naming %s and NUL", c.what, status, out, c.field)
		}
		mcpErr := h.mcpCall(c.op, c.body).toolError()
		if mcpErr == nil || mcpErr["code"] != "invalid_request" || mcpErr["message"] != msg {
			t.Fatalf("%s: MCP answered differently: %v vs %q", c.what, mcpErr, msg)
		}
	}

	data := h.mustHTTP("query", map[string]any{"namespace": "nul", "sql": "SELECT count(*) AS n FROM notes"})
	if n := int64val(t, "n", data["rows"].([]any)[0].(map[string]any)["n"]); n != 1 {
		t.Fatalf("a refused NUL write changed the table: %d rows", n)
	}

	res := h.subscribe(t, url.Values{"namespace": {"nul"}, "cursor": {"a\x00b"}})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("subscribe with a NUL cursor: status %d, want 400", res.StatusCode)
	}
}

func TestTheGoLibraryRefusesANulInsideAJSONValue(t *testing.T) {
	emb := openEmbedded(t, t.TempDir())
	defer emb.Close()
	ctx := context.Background()
	if err := emb.CreateNamespace(ctx, "nul"); err != nil {
		t.Fatal(err)
	}
	if _, err := emb.CreateTable(ctx, "nul", "notes", []dolmen.Field{{Name: "prefs", Type: "json"}}); err != nil {
		t.Fatal(err)
	}
	_, err := emb.Insert(ctx, "nul", "notes", []map[string]any{{"prefs": map[string]any{"k": "a\x00b"}}}, dolmen.InsertOptions{})
	if !errors.Is(err, dolmen.ErrInvalidRequest) || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("a NUL inside a json value must be invalid_request: %v", err)
	}
}
