package api

import "testing"

func assertDeclared(t *testing.T, where string, sc map[string]any, got map[string]any) {
	t.Helper()
	props, ok := sc["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: the advertised schema declares no properties at all: %v", where, sc)
	}
	for key := range got {
		if _, ok := props[key]; !ok {
			t.Fatalf("%s carries %q, which /v1/openapi.json and tools/list never declare, so a client validating a real response against the advertised schema rejects it", where, key)
		}
	}
}

func TestTheAdvertisedMigrateSchemaDeclaresWhatAMigrationReturns(t *testing.T) {
	srv := newTestServer(t)
	mustNS(t, srv.URL, "adv")
	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "adv",
		"table":     "notes",
		"fields": []map[string]any{
			{"name": "body", "type": "text", "fulltext": true},
			{"name": "scratch", "type": "string"},
		},
	})
	if code != 200 {
		t.Fatalf("create table: %d %v", code, res)
	}
	if code, res := post(t, srv.URL, "insert", map[string]any{
		"namespace": "adv", "table": "notes",
		"records": []map[string]any{{"body": "a note", "scratch": "x"}},
	}); code != 200 {
		t.Fatalf("seed insert: %d %v", code, res)
	}

	changes := []map[string]any{
		{"op": "add_field", "field": map[string]any{"name": "tag", "type": "string", "required": true}, "default": "none"},
		{"op": "drop_field", "name": "scratch"},
	}
	code, res = post(t, srv.URL, "migrate", map[string]any{
		"namespace": "adv", "table": "notes", "changes": changes, "dry_run": true, "expected_version": 1,
	})
	if code != 200 {
		t.Fatalf("dry run: %d %v", code, res)
	}

	advertised := Ops["migrate"].OutputSchema
	data, _ := res["data"].(map[string]any)
	assertDeclared(t, "a dry run response", advertised, data)

	plan, _ := data["plan"].(map[string]any)
	if len(plan) == 0 {
		t.Fatalf("a dry run must return a plan, so this test has something to check: %v", data)
	}
	planSchema, _ := advertised["properties"].(map[string]any)["plan"].(map[string]any)
	assertDeclared(t, "a dry run plan", planSchema, plan)

	for _, key := range []string{"expected_incarnation", "backfill_rows", "destructive", "rebuild_fulltext"} {
		if _, ok := plan[key]; !ok {
			t.Fatalf("the plan carries no %q, so this migration exercises less of the schema than it means to: %v", key, plan)
		}
	}

	code, res = post(t, srv.URL, "migrate", map[string]any{
		"namespace": "adv", "table": "notes", "changes": changes,
		"expected_incarnation": plan["expected_incarnation"], "expected_version": 1,
	})
	if code != 200 {
		t.Fatalf("applying the plan the dry run described: %d %v", code, res)
	}
	data, _ = res["data"].(map[string]any)
	assertDeclared(t, "an applied migration response", advertised, data)
}
