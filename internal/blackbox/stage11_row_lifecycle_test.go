package blackbox

import (
	"strings"
	"testing"
)

const stage11Namespace = "acme/lifecycle"

const readRowsIDsTeaching = `ids is required (pass the ids a write returned, a query projected, or a change feed carried; an empty list selects nothing)`

func TestStage11RowLifecycleAndPinnedErrors(t *testing.T) {
	op(t, "create_namespace", map[string]any{"namespace": stage11Namespace})
	op(t, "create_table", map[string]any{
		"namespace": stage11Namespace,
		"table":     "docs",
		"fields": []any{
			map[string]any{"name": "title", "type": "string", "fulltext": true},
			map[string]any{"name": "hours", "type": "number"},
		},
	})

	ins := op(t, "insert", map[string]any{
		"namespace": stage11Namespace,
		"table":     "docs",
		"records": []any{
			map[string]any{"title": "alpha", "hours": 1.5},
			map[string]any{"title": "beta"},
		},
	})
	insIDs, _ := ins["ids"].([]any)
	if len(insIDs) != 2 {
		t.Fatalf("insert returned %v", insIDs)
	}
	first := asInt(t, insIDs[0], "insert id 0")
	second := asInt(t, insIDs[1], "insert id 1")

	read := op(t, "read_rows", map[string]any{
		"namespace": stage11Namespace,
		"table":     "docs",
		"ids":       []any{second, 900, first},
	})
	rows := asMapList(t, read["rows"], "read_rows rows")
	if len(rows) != 2 || asInt(t, rows[0]["id"], "row 0 id") != first || asInt(t, rows[1]["id"], "row 1 id") != second {
		t.Fatalf("read_rows must return found rows in ascending id order and drop missing ids: %v", rows)
	}
	hours, haveHours := rows[1]["hours"]
	if !haveHours || hours != nil {
		t.Fatalf("an omitted number field must read back as an explicit null key, got %v (present=%v)", hours, haveHours)
	}

	code, envelope := opErrorEnvelope(t, "read_rows", map[string]any{"namespace": stage11Namespace, "table": "docs"})
	if code != 400 {
		t.Fatalf("read_rows without ids: HTTP %d", code)
	}
	assertTeachingError(t, envelope, "read_rows missing ids", readRowsIDsTeaching)

	empty := op(t, "read_rows", map[string]any{"namespace": stage11Namespace, "table": "docs", "ids": []any{}})
	if rc := asInt(t, empty["row_count"], "empty-set row_count"); rc != 0 {
		t.Fatalf("an explicit empty id list selects nothing: %v", empty)
	}
	if rowsAgain, _ := empty["rows"].([]any); len(rowsAgain) != 0 {
		t.Fatalf("an explicit empty id list must return an empty rows array: %v", empty)
	}

	updated := op(t, "update", map[string]any{
		"namespace": stage11Namespace,
		"table":     "docs",
		"filter":    "title = ?",
		"args":      []any{"beta"},
		"set":       map[string]any{"hours": 2.5},
	})
	if n := asInt(t, updated["updated"], "update updated"); n != 1 {
		t.Fatalf("update by filter: %v", updated)
	}

	deleted := op(t, "delete", map[string]any{
		"namespace": stage11Namespace,
		"table":     "docs",
		"filter":    "title = ?",
		"args":      []any{"alpha"},
	})
	if n := asInt(t, deleted["deleted"], "delete deleted"); n != 1 {
		t.Fatalf("delete by filter: %v", deleted)
	}
	if m := asInt(t, deleted["matched"], "delete matched"); m != 1 {
		t.Fatalf("delete matched count: %v", deleted)
	}

	filterUpsert := op(t, "upsert", map[string]any{
		"namespace": stage11Namespace,
		"table":     "docs",
		"filter":    "title = ?",
		"args":      []any{"no-such-row"},
		"set":       map[string]any{"title": "gamma"},
	})
	if inserted := asInt(t, filterUpsert["inserted"], "upsert inserted"); inserted != 1 {
		t.Fatalf("upsert with no filter match must insert: %v", filterUpsert)
	}

	keyUpsert := op(t, "upsert_by_key", map[string]any{
		"namespace": stage11Namespace,
		"table":     "docs",
		"on":        []any{"title"},
		"records":   []any{map[string]any{"title": "beta", "hours": 3.5}},
	})
	if n := asInt(t, keyUpsert["updated"], "upsert_by_key updated"); n != 1 {
		t.Fatalf("upsert_by_key partial update: %v", keyUpsert)
	}
	state := op(t, "query", map[string]any{
		"namespace": stage11Namespace,
		"sql":       "SELECT title, hours FROM docs ORDER BY title",
	})
	stateRows := asMapList(t, state["rows"], "docs rows")
	if len(stateRows) != 2 {
		t.Fatalf("docs holds %d rows after the lifecycle, expected 2: %v", len(stateRows), stateRows)
	}
	if stateRows[0]["title"] != "beta" || stateRows[0]["hours"] != 3.5 || stateRows[1]["title"] != "gamma" {
		t.Fatalf("the lifecycle left unexpected rows: %v", stateRows)
	}

	code, envelope = opErrorEnvelope(t, "read_rows", map[string]any{"namespace": stage11Namespace, "table": "docs", "ids": "abc"})
	if code != 400 {
		t.Fatalf("type-mismatched ids: HTTP %d", code)
	}
	errObj, _ := envelope["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("type-mismatch envelope carries no error object: %v", envelope)
	}
	mismatchMsg := asStr(t, errObj["message"], "type-mismatch message")
	if !strings.Contains(mismatchMsg, "invalid JSON") || !strings.Contains(mismatchMsg, "cannot unmarshal") {
		t.Fatalf("issue #287 pin: envelope type mismatches still lead with invalid JSON and the raw decoder text, got %q", mismatchMsg)
	}

	code, _, raw := postRawBody(t, app.srv.url+"/v1/insert", nil, `{"namespace":"acme/lifecycle","table":"docs","records":[{"title":"nf","hours":1e999}]}`)
	var nonFinite map[string]any
	decodeInto(t, raw, &nonFinite, "non-finite envelope")
	errObj, _ = nonFinite["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("non-finite envelope carries no error object: %v", nonFinite)
	}
	if code != 400 || asStr(t, errObj["code"], "non-finite code") != "invalid_request" {
		t.Fatalf("non-finite numbers must be rejected as invalid_request, got %d %v", code, nonFinite)
	}
	if msg := asStr(t, errObj["message"], "non-finite message"); !strings.Contains(msg, `field "hours": expected a number`) {
		t.Fatalf("non-finite rejection must name the field, got %q", msg)
	}

	code, envelope = opErrorEnvelope(t, "drop_table", map[string]any{"namespace": stage11Namespace, "table": "docs"})
	if code != 400 {
		t.Fatalf("drop_table without confirm: HTTP %d", code)
	}
	assertTeachingError(t, envelope, "drop_table unconfirmed", `confirm must repeat the exact table name "docs" to drop it`)

	op(t, "drop_table", map[string]any{"namespace": stage11Namespace, "table": "docs", "confirm": "docs"})
	recreated := op(t, "create_table", map[string]any{
		"namespace": stage11Namespace,
		"table":     "docs",
		"fields":    []any{map[string]any{"name": "title", "type": "string"}},
	})
	table, _ := recreated["table"].(map[string]any)
	if table == nil || asInt(t, table["version"], "recreated version") != 1 {
		t.Fatalf("a dropped and recreated table must start at version 1: %v", recreated)
	}

	normalized := op(t, "create_table", map[string]any{
		"namespace": stage11Namespace,
		"table":     "Notes",
		"fields":    []any{map[string]any{"name": "title", "type": "string"}},
	})
	normTable, _ := normalized["table"].(map[string]any)
	if normTable == nil {
		t.Fatalf("create_table returned no table object: %v", normalized)
	}
	if name := asStr(t, normTable["name"], "normalized name"); name != "notes" {
		t.Fatalf("mixed-case table names are normalized to lowercase, got %q", name)
	}

	code, envelope = opErrorEnvelope(t, "read_rows", map[string]any{"namespace": stage11Namespace, "table": "sqlite_meta", "ids": []any{1}})
	if code != 404 {
		t.Fatalf("a reserved table prefix must be not_found, got %d %v", code, envelope)
	}
	errObj, _ = envelope["error"].(map[string]any)
	if errObj == nil || asStr(t, errObj["code"], "reserved-table code") != "not_found" {
		t.Fatalf("a reserved table prefix must read not_found, got %v", envelope)
	}

	caps := op(t, "capabilities", map[string]any{})
	if exec := asStr(t, caps["vector_execution"], "capabilities vector_execution"); exec != "exact" {
		t.Fatalf("capabilities vector_execution: %v", caps)
	}
	if subscribe, _ := caps["subscribe"].(bool); !subscribe {
		t.Fatalf("capabilities must report subscribe: %v", caps)
	}

	listed := op(t, "list_namespaces", map[string]any{})
	rawNS, _ := listed["namespaces"].([]any)
	found := false
	for _, ns := range rawNS {
		if s, ok := ns.(string); ok && s == stage11Namespace {
			found = true
		}
	}
	if !found {
		t.Fatalf("list_namespaces does not report %s: %v", stage11Namespace, rawNS)
	}

	res, err := mcpRaw("tools/call", map[string]any{
		"name":      "read_rows",
		"arguments": map[string]any{"namespace": stage11Namespace, "table": "docs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "ids is required") {
		t.Fatalf("mcp read_rows without ids must carry the teaching error, got %+v", res)
	}
}
