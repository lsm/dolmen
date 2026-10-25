package conformance

import (
	"net/http"
	"strings"
	"testing"
)

func constraintTables(h *harness) {
	h.t.Helper()
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

func TestConstraintMigrationsOnBothTransports(t *testing.T) {
	h := newHarness(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "notes", "fields": []map[string]any{{"name": "slug", "type": "string"}, {"name": "parent_id", "type": "number"}}})
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"slug": "dup"}, {"slug": "dup"}}})
	unique := map[string]any{"op": "add_constraint", "constraint": map[string]any{"name": "slug_key", "kind": "unique", "fields": []string{"slug"}}}
	body := map[string]any{"namespace": "app", "table": "notes", "changes": []any{unique}}
	code, out := h.httpCall("migrate", body)
	if code != http.StatusBadRequest || errCode(out) != "conflict" || !strings.Contains(errMessage(out), "slug_key") {
		t.Fatalf("http add_constraint over rows that break it: %d %v", code, out)
	}
	if env := h.mcpCall("migrate", body).toolError(); env["code"] != "conflict" {
		t.Fatalf("mcp add_constraint over rows that break it: %v", env)
	}
	h.mustHTTP("delete", map[string]any{"namespace": "app", "table": "notes", "filter": "id = 2"})
	h.mustMCP("migrate", body)
	h.mustMCP("migrate", map[string]any{"namespace": "app", "table": "notes", "changes": []any{map[string]any{"op": "set_accept_references", "value": true}}})
	h.mustHTTP("migrate", map[string]any{"namespace": "app", "table": "notes", "changes": []any{map[string]any{"op": "add_constraint",
		"constraint": map[string]any{"name": "parent_ref", "kind": "foreign_key", "field": "parent_id", "references": map[string]any{"table": "notes", "field": "id"}}}}})
	code, out = h.httpCall("insert", map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"slug": "child", "parent_id": 9}}})
	if code != http.StatusBadRequest || errCode(out) != "conflict" || !strings.Contains(errMessage(out), "parent_ref") {
		t.Fatalf("a self-reference to a missing row: %d %v", code, out)
	}
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"slug": "child", "parent_id": 1}}})
	code, out = h.httpCall("migrate", map[string]any{"namespace": "app", "table": "notes", "changes": []any{map[string]any{"op": "drop_field", "name": "parent_id"}}, "expected_version": 4})
	if code != http.StatusBadRequest || errCode(out) != "invalid_request" || !strings.Contains(errMessage(out), "parent_ref") {
		t.Fatalf("dropping a field a constraint names: %d %v", code, out)
	}
	h.mustMCP("migrate", map[string]any{"namespace": "app", "table": "notes", "changes": []any{map[string]any{"op": "drop_constraint", "name": "parent_ref"}}})
	desc := h.mustHTTP("describe_table", map[string]any{"namespace": "app", "table": "notes"})["table"].(map[string]any)
	if cs, _ := desc["constraints"].([]any); len(cs) != 1 || cs[0].(map[string]any)["name"] != "slug_key" || int64val(t, "version", desc["version"]) != 5 {
		t.Fatalf("after drop_constraint: %v", desc)
	}
	history := h.mustMCP("list_migrations", map[string]any{"namespace": "app", "table": "notes"})
	if !strings.Contains(mustJSON(t, history), `"slug_key"`) {
		t.Fatalf("list_migrations must record the added constraint: %v", history)
	}
}

func TestConstraintMigrationValidationLeavesSchemaUnchanged(t *testing.T) {
	h := newHarness(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "notes", "fields": []map[string]any{{"name": "slug", "type": "string"}}})
	for label, changes := range map[string][]any{
		"missing constraint":     {map[string]any{"op": "add_constraint"}},
		"unknown constraint key": {map[string]any{"op": "add_constraint", "constraint": map[string]any{"name": "slug_key", "kind": "unique", "fields": []string{"slug"}, "typo": true}}},
		"missing name":           {map[string]any{"op": "drop_constraint"}},
		"missing value":          {map[string]any{"op": "set_accept_references"}},
		"null value":             {map[string]any{"op": "set_accept_references", "value": nil}},
		"irrelevant key":         {map[string]any{"op": "drop_constraint", "name": "slug_key", "value": true}},
		"mixed changes":          {map[string]any{"op": "set_accept_references", "value": true}, map[string]any{"op": "set_enum", "name": "slug", "enum": []string{"a"}}},
	} {
		body := map[string]any{"namespace": "app", "table": "notes", "changes": changes}
		if code, out := h.httpCall("migrate", body); code != http.StatusBadRequest || errCode(out) != "invalid_request" {
			t.Fatalf("http %s: %d %v", label, code, out)
		}
		if env := h.mcpCall("migrate", body).toolError(); env["code"] != "invalid_request" {
			t.Fatalf("mcp %s: %v", label, env)
		}
	}
	dry := map[string]any{"namespace": "app", "table": "notes", "dry_run": true, "changes": []any{map[string]any{"op": "add_constraint", "constraint": map[string]any{"name": "slug_key", "kind": "unique", "fields": []string{"slug"}}}}}
	h.mustHTTP("migrate", dry)
	h.mustMCP("migrate", dry)
	desc := h.mustHTTP("describe_table", map[string]any{"namespace": "app", "table": "notes"})["table"].(map[string]any)
	cs, _ := desc["constraints"].([]any)
	if int64val(t, "version", desc["version"]) != 1 || desc["accept_references"] == true || len(cs) != 0 {
		t.Fatalf("refusals or dry runs changed the schema: %v", desc)
	}
}

func TestConstraintMigrationsAvailableWithAuthOn(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "notes", "fields": []map[string]any{{"name": "slug", "type": "string"}}})
	for _, change := range []map[string]any{
		{"op": "add_constraint", "constraint": map[string]any{"name": "slug_key", "kind": "unique", "fields": []string{"slug"}}},
		{"op": "drop_constraint", "name": "slug_key"},
		{"op": "set_accept_references", "value": true},
	} {
		body := map[string]any{"namespace": "app", "table": "notes", "changes": []any{change}}
		body["dry_run"] = true
		h.mustHTTP("migrate", body)
		body["dry_run"] = false
		h.mustMCP("migrate", body)
	}
}
