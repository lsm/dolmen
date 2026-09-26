package conformance

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	dolmen "github.com/lsm/dolmen"
)

func seedShapedNotes(t *testing.T, h *harness) {
	t.Helper()
	h.seedTable("shp", "notes", []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "tags", "type": "json", "shape": "array<string>"},
		{"name": "meta", "type": "json", "shape": "object"},
	})
}

func wantShapeRefusal(t *testing.T, h *harness, what, op string, body map[string]any, fragments ...string) {
	t.Helper()
	status, out := h.httpCall(op, body)
	if status != http.StatusBadRequest {
		t.Fatalf("%s: status %d, want 400: %v", what, status, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if code, _ := errEnv["code"].(string); code != "invalid_request" {
		t.Fatalf("%s: code %v, want invalid_request", what, errEnv["code"])
	}
	msg, _ := errEnv["message"].(string)
	for _, f := range fragments {
		if !strings.Contains(msg, f) {
			t.Fatalf("%s: the refusal %q must mention %q", what, msg, f)
		}
	}
	res := h.mcpCall(op, body)
	mcpErr := res.toolError()
	if mcpErr == nil {
		t.Fatalf("%s: MCP accepted what /v1 refused", what)
	}
	if mcpErr["code"] != errEnv["code"] || mcpErr["message"] != errEnv["message"] {
		t.Fatalf("%s: MCP refused differently: %v vs %v", what, mcpErr, errEnv)
	}
}

func TestJSONShapeIsEnforcedOnEveryWrite(t *testing.T) {
	h := newHarness(t)
	seedShapedNotes(t, h)

	h.mustHTTP("insert", map[string]any{"namespace": "shp", "table": "notes", "records": []map[string]any{
		{"title": "ok", "tags": []any{"db", "sqlite"}, "meta": map[string]any{"k": "v"}},
		{"title": "empty", "tags": []any{}},
		{"title": "unset"},
	}})

	ins := func(rec map[string]any) map[string]any {
		return map[string]any{"namespace": "shp", "table": "notes", "records": []map[string]any{rec}}
	}
	wantShapeRefusal(t, h, "a bare string for array<string>", "insert", ins(map[string]any{"title": "bad", "tags": "db,sqlite"}),
		`"tags"`, "array<string>", "a string", `["a","b"]`)
	wantShapeRefusal(t, h, "a number inside array<string>", "insert", ins(map[string]any{"title": "bad", "tags": []any{"db", 7}}),
		`"tags"`, "a number at index 1")
	wantShapeRefusal(t, h, "an array for object", "insert", ins(map[string]any{"title": "bad", "meta": []any{"x"}}),
		`"meta"`, "object", "an array")
	wantShapeRefusal(t, h, "update", "update", map[string]any{"namespace": "shp", "table": "notes", "filter": "title = ?", "args": []any{"ok"}, "set": map[string]any{"tags": "x"}},
		`"tags"`, "array<string>")
	wantShapeRefusal(t, h, "upsert", "upsert", map[string]any{"namespace": "shp", "table": "notes", "filter": "title = ?", "args": []any{"new"}, "set": map[string]any{"title": "new", "tags": map[string]any{"a": 1}}},
		`"tags"`, "an object")
	wantShapeRefusal(t, h, "upsert_by_key", "upsert_by_key", map[string]any{"namespace": "shp", "table": "notes", "on": []string{"title"}, "records": []map[string]any{{"title": "ok", "tags": true}}},
		`"tags"`, "a boolean")

	data := h.mustHTTP("query", map[string]any{"namespace": "shp", "sql": "SELECT count(*) AS n FROM notes"})
	rows, _ := data["rows"].([]any)
	if n := int64val(t, "n", rows[0].(map[string]any)["n"]); n != 3 {
		t.Fatalf("refused writes changed the table: %d rows, want 3", n)
	}

	described := h.mustHTTP("describe_table", map[string]any{"namespace": "shp", "table": "notes"})
	assertJSONEqual(t, "describe_table over MCP vs HTTP", h.mustMCP("describe_table", map[string]any{"namespace": "shp", "table": "notes"}), described)
	table, _ := described["table"].(map[string]any)
	fields, _ := table["fields"].([]any)
	shapes := map[string]any{}
	for _, f := range fields {
		m := f.(map[string]any)
		shapes[m["name"].(string)] = m["shape"]
	}
	if shapes["tags"] != "array<string>" || shapes["meta"] != "object" || shapes["title"] != nil {
		t.Fatalf("describe_table must report each field's shape: %v", shapes)
	}
}

func TestJSONShapeDeclarationsAreChecked(t *testing.T) {
	h := newHarness(t)
	h.ensureNS("shp")
	create := func(field map[string]any) map[string]any {
		return map[string]any{"namespace": "shp", "table": "t", "fields": []map[string]any{field}}
	}
	wantShapeRefusal(t, h, "shape on a string field", "create_table", create(map[string]any{"name": "tags", "type": "string", "shape": "array<string>"}),
		"shape")
	wantShapeRefusal(t, h, "a default that does not fit", "create_table", create(map[string]any{"name": "tags", "type": "json", "shape": "array<string>", "default": "none"}),
		`"tags"`, "array<string>")
}

func TestSetShapeChecksStoredRows(t *testing.T) {
	h := newHarness(t)
	h.seedTable("shp", "notes", []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "tags", "type": "json"},
	})
	h.mustHTTP("insert", map[string]any{"namespace": "shp", "table": "notes", "records": []map[string]any{
		{"title": "good", "tags": []any{"a"}},
		{"title": "legacy", "tags": "a,b"},
		{"title": "unset"},
	}})
	migrate := func(shape any) map[string]any {
		return map[string]any{"namespace": "shp", "table": "notes", "changes": []map[string]any{{"op": "set_shape", "name": "tags", "shape": shape}}}
	}

	wantShapeRefusal(t, h, "a shape stored rows do not fit", "migrate", migrate("array<string>"),
		`"tags"`, "array<string>", "1 rows", "ids 2")
	wantShapeRefusal(t, h, "set_shape on a string field", "migrate",
		map[string]any{"namespace": "shp", "table": "notes", "changes": []map[string]any{{"op": "set_shape", "name": "title", "shape": "object"}}},
		`"title"`, "json")
	wantShapeRefusal(t, h, "an unknown shape", "migrate", migrate("list<string>"), "array<string>")
	wantShapeRefusal(t, h, "set_shape without a shape", "migrate",
		map[string]any{"namespace": "shp", "table": "notes", "changes": []map[string]any{{"op": "set_shape", "name": "tags"}}},
		"set_shape")

	h.mustHTTP("update", map[string]any{"namespace": "shp", "table": "notes", "filter": "title = ?", "args": []any{"legacy"}, "set": map[string]any{"tags": []any{"a", "b"}}})
	h.mustHTTP("migrate", migrate("array<string>"))
	wantShapeRefusal(t, h, "a write after set_shape", "insert",
		map[string]any{"namespace": "shp", "table": "notes", "records": []map[string]any{{"title": "late", "tags": "c"}}},
		`"tags"`, "array<string>")

	h.mustHTTP("migrate", migrate(""))
	h.mustHTTP("insert", map[string]any{"namespace": "shp", "table": "notes", "records": []map[string]any{{"title": "free", "tags": "anything"}}})

	history := h.mustHTTP("list_migrations", map[string]any{"namespace": "shp", "table": "notes"})
	if !strings.Contains(mustJSON(t, history), `"set_shape"`) {
		t.Fatalf("list_migrations must record set_shape: %v", history)
	}
}

func TestJSONShapeHoldsInTheGoLibrary(t *testing.T) {
	emb := openEmbedded(t, t.TempDir())
	defer emb.Close()
	ctx := context.Background()
	if err := emb.CreateNamespace(ctx, "shp"); err != nil {
		t.Fatal(err)
	}
	sc, err := emb.CreateTable(ctx, "shp", "notes", []dolmen.Field{{Name: "tags", Type: "json", Shape: "array<string>"}})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Fields[0].Shape != "array<string>" {
		t.Fatalf("the facade lost the shape: %+v", sc.Fields[0])
	}
	if _, err := emb.Insert(ctx, "shp", "notes", []map[string]any{{"tags": []string{"a", "b"}}}, dolmen.InsertOptions{}); err != nil {
		t.Fatalf("a Go []string must fit array<string>: %v", err)
	}
	_, err = emb.Insert(ctx, "shp", "notes", []map[string]any{{"tags": "a,b"}}, dolmen.InsertOptions{})
	if !errors.Is(err, dolmen.ErrInvalidRequest) || !strings.Contains(err.Error(), "array<string>") {
		t.Fatalf("the facade must refuse a misshapen value as invalid: %v", err)
	}
}
