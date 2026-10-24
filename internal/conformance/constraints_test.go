package conformance

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func declaringEngine(t *testing.T) {
	t.Helper()
	if testEngine(t) != store.EngineSQLite {
		t.Skip("only SQLite keeps constraint declarations so far; remove this skip as each engine's create slice lands")
	}
}

func constraintTables(h *harness) {
	h.t.Helper()
	declaringEngine(h.t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "projects", "accept_references": true,
		"fields": []map[string]any{{"name": "slug", "type": "string"}}})
	h.mustMCP("create_table", map[string]any{"namespace": "app", "table": "tasks",
		"fields": []map[string]any{{"name": "project_id", "type": "number"}, {"name": "slug", "type": "string"}, {"name": "done", "type": "boolean"}},
		"constraints": []map[string]any{
			{"name": "project_ref", "kind": "foreign_key", "field": "project_id", "references": map[string]any{"table": "projects", "field": "id"}},
			{"name": "open_slug", "kind": "unique", "fields": []string{"slug"}, "where": "done = FALSE"},
		}})
}

func TestConstraintsRoundTripThroughDescribeOnBothTransports(t *testing.T) {
	h := newHarness(t)
	constraintTables(h)
	for transport, out := range map[string]map[string]any{
		"http": h.mustHTTP("describe_table", map[string]any{"namespace": "app", "table": "tasks"}),
		"mcp":  h.mustMCP("describe_table", map[string]any{"namespace": "app", "table": "tasks"}),
	} {
		cs, _ := out["table"].(map[string]any)["constraints"].([]any)
		if len(cs) != 2 || cs[0].(map[string]any)["name"] != "project_ref" || cs[1].(map[string]any)["where"] != "done = FALSE" {
			t.Fatalf("%s: describe_table lost the declarations: %v", transport, out)
		}
	}
	parent := h.mustMCP("describe_table", map[string]any{"namespace": "app", "table": "projects"})
	if parent["table"].(map[string]any)["accept_references"] != true {
		t.Fatalf("describe_table must report accept_references: %v", parent)
	}
}

func TestConstraintViolationsAreConflictsThatNameTheConstraint(t *testing.T) {
	h := newHarness(t)
	constraintTables(h)
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "projects", "records": []map[string]any{{"slug": "p"}}})
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "tasks", "records": []map[string]any{{"project_id": 1, "slug": "a", "done": false}}})
	for name, rec := range map[string]map[string]any{
		"open_slug":   {"project_id": 1, "slug": "a", "done": false},
		"project_ref": {"project_id": 2, "slug": "b", "done": false},
	} {
		body := map[string]any{"namespace": "app", "table": "tasks", "records": []map[string]any{rec}}
		code, out := h.httpCall("insert", body)
		if code != http.StatusBadRequest || errCode(out) != "conflict" || !strings.Contains(errMessage(out), `constraint "`+name+`"`) {
			t.Fatalf("http %s: %d %v", name, code, out)
		}
		if env := h.mcpCall("insert", body).toolError(); env["code"] != "conflict" || env["message"] != errMessage(out) {
			t.Fatalf("mcp %s: %v", name, env)
		}
	}
	h.mustMCP("insert", map[string]any{"namespace": "app", "table": "tasks", "records": []map[string]any{{"project_id": 1, "slug": "a", "done": true}, {"slug": "loose", "done": false}}})
	code, out := h.httpCall("delete", map[string]any{"namespace": "app", "table": "projects", "filter": "id = 1"})
	if code != http.StatusBadRequest || errCode(out) != "conflict" || !strings.Contains(errMessage(out), "project_ref") {
		t.Fatalf("deleting a referenced parent: %d %v", code, out)
	}
	code, out = h.httpCall("drop_table", map[string]any{"namespace": "app", "table": "projects", "confirm": "projects"})
	if code != http.StatusBadRequest || errCode(out) != "invalid_request" || !strings.Contains(errMessage(out), "tasks") {
		t.Fatalf("dropping a referenced table: %d %v", code, out)
	}
	rows := h.mustHTTP("read_rows", map[string]any{"namespace": "app", "table": "tasks"})["rows"].([]any)
	if len(rows) != 3 {
		t.Fatalf("refused writes changed rows: %v", rows)
	}
}

func TestInvalidConstraintDeclarationsCreateNothing(t *testing.T) {
	h := newHarness(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "plain", "fields": []map[string]any{{"name": "slug", "type": "string"}}})
	for label, c := range map[string]map[string]any{
		"not opted in":  {"name": "plain_ref", "kind": "foreign_key", "field": "ref", "references": map[string]any{"table": "plain", "field": "id"}},
		"json key":      {"name": "doc_key", "kind": "unique", "fields": []string{"doc"}},
		"function call": {"name": "slug_key", "kind": "unique", "fields": []string{"slug"}, "where": "lower(slug) = 'x'"},
	} {
		code, out := h.httpCall("create_table", map[string]any{"namespace": "app", "table": "notes",
			"fields":      []map[string]any{{"name": "slug", "type": "string"}, {"name": "ref", "type": "number"}, {"name": "doc", "type": "json"}},
			"constraints": []map[string]any{c}})
		if code != http.StatusBadRequest || errCode(out) != "invalid_request" {
			t.Fatalf("%s: %d %v", label, code, out)
		}
		if code, out := h.httpCall("describe_table", map[string]any{"namespace": "app", "table": "notes"}); code != http.StatusNotFound {
			t.Fatalf("%s left a table behind: %d %v", label, code, out)
		}
	}
}
