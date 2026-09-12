package conformance

import (
	"net/http"
	"testing"
)

func TestNamespaceCreatedOnFirstUse(t *testing.T) {
	h := newHarness(t)

	status, body := h.httpCall("insert", map[string]any{
		"namespace": "firstuse", "table": "t",
		"records": []map[string]any{{"a": "x"}},
	})
	if status != http.StatusNotFound {
		t.Fatalf("insert into a missing table of a missing namespace: status %d, want 404: %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "not_found" {
		t.Fatalf("expected a not_found envelope, got %v", body)
	}
	msg, _ := errObj["message"].(string)
	if msg == "" {
		t.Fatalf("message must be a non-empty string: %v", errObj)
	}
	wantMessage(t, "insert names the table", msg, `firstuse\.t`)

	data := h.mustHTTP("list_namespaces", map[string]any{})
	nss, _ := data["namespaces"].([]any)
	found := false
	for _, ns := range nss {
		if ns == "firstuse" {
			found = true
		}
	}
	if !found {
		t.Fatalf("namespace firstuse must exist after first use: %v", nss)
	}

	tables := h.mustHTTP("list_tables", map[string]any{"namespace": "firstuse"})
	if ts, _ := tables["tables"].([]any); len(ts) != 0 {
		t.Fatalf("a freshly created namespace lists no tables: %v", ts)
	}
}

func TestNamespaceTreeListingAndLeafOnlyDrops(t *testing.T) {
	h := newHarness(t)

	for _, ns := range []string{"acme", "acme/prod", "acme/prod/eu", "acme/stage", "acme2", "edge", "edge-x", "zeta"} {
		h.ensureNS(ns)
	}

	data := h.mustHTTP("list_namespaces", map[string]any{})
	assertJSONEqual(t, "full recursive listing", data["namespaces"],
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage", "acme2", "edge-x", "edge", "zeta"})

	data = h.mustHTTP("list_namespaces", map[string]any{"prefix": "acme"})
	assertJSONEqual(t, "prefix listing", data["namespaces"],
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage"})
	data = h.mustHTTP("list_namespaces", map[string]any{"prefix": "acme/prod"})
	assertJSONEqual(t, "deep prefix listing", data["namespaces"],
		[]any{"acme/prod", "acme/prod/eu"})

	sc := h.mustMCP("list_namespaces", map[string]any{"prefix": " ACME "})
	assertJSONEqual(t, "normalized MCP prefix listing", sc["namespaces"],
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage"})
	data = h.mustHTTP("list_namespaces", map[string]any{"prefix": "ghost"})
	assertJSONEqual(t, "absent prefix listing", data["namespaces"], []any{})

	status, body := h.httpCall("list_namespaces", map[string]any{"prefix": "acme/prod/eu/deep"})
	if status != http.StatusBadRequest {
		t.Fatalf("over-depth prefix: status %d, want 400: %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid_request" {
		t.Fatalf("over-depth prefix: expected an invalid_request envelope, got %v", body)
	}

	for _, prefix := range []string{"", "   "} {
		status, body = h.httpCall("list_namespaces", map[string]any{"prefix": prefix})
		if status != http.StatusBadRequest {
			t.Fatalf("empty prefix %q: status %d, want 400: %v", prefix, status, body)
		}
		errObj, _ = body["error"].(map[string]any)
		if errObj == nil || errObj["code"] != "invalid_request" {
			t.Fatalf("empty prefix %q: expected an invalid_request envelope, got %v", prefix, body)
		}
	}

	status, body = h.httpCall("drop_namespace", map[string]any{"namespace": "acme", "confirm": "acme"})
	if status != http.StatusBadRequest {
		t.Fatalf("parent drop: status %d, want 400: %v", status, body)
	}
	errObj, _ = body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid_request" {
		t.Fatalf("parent drop: expected an invalid_request envelope, got %v", body)
	}
	msg, _ := errObj["message"].(string)
	wantMessage(t, "parent drop names the descendant count", msg, `namespace acme has 3 descendant namespaces`)
	status, body = h.httpCall("drop_namespace", map[string]any{"namespace": "acme/prod", "confirm": "acme/prod"})
	if status != http.StatusBadRequest {
		t.Fatalf("mid-tree drop must be refused: %d %v", status, body)
	}
	errObj, _ = body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid_request" {
		t.Fatalf("mid-tree drop: expected an invalid_request envelope, got %v", body)
	}
	msg, _ = errObj["message"].(string)
	wantMessage(t, "mid-tree drop names its 1 descendant", msg, `namespace acme/prod has 1 descendant namespace`)
	data = h.mustHTTP("list_namespaces", map[string]any{})
	assertJSONEqual(t, "refused drops deleted nothing", data["namespaces"],
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage", "acme2", "edge-x", "edge", "zeta"})

	for _, ns := range []string{"acme/prod/eu", "acme/prod", "acme/stage", "acme"} {
		h.mustHTTP("drop_namespace", map[string]any{"namespace": ns, "confirm": ns})
	}
	data = h.mustHTTP("list_namespaces", map[string]any{})
	assertJSONEqual(t, "post-drop listing", data["namespaces"], []any{"acme2", "edge-x", "edge", "zeta"})
}

func TestNamespaceDeepPathLifecycle(t *testing.T) {
	h := newHarness(t)

	data := h.mustHTTP("create_table", map[string]any{
		"namespace": "acme/prod",
		"table":     "findings",
		"fields":    []map[string]any{{"name": "title", "type": "string"}},
	})
	tbl, _ := data["table"].(map[string]any)
	if tbl["namespace"] != "acme/prod" {
		t.Fatalf("created table reports its namespace path, got %v", tbl["namespace"])
	}

	h.mustHTTP("insert", map[string]any{
		"namespace": "acme/prod",
		"table":     "findings",
		"records":   []map[string]any{{"title": "auth flow"}},
	})
	sc := h.mustMCP("query", map[string]any{
		"namespace": "acme/prod",
		"sql":       "SELECT title FROM findings",
	})
	if rc, _ := sc["row_count"].(float64); rc != 1 {
		t.Fatalf("deep-path query row_count, want 1: %v", sc)
	}
	rows, _ := sc["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("deep-path query rows, want 1: %v", sc["rows"])
	}
	if row, _ := rows[0].(map[string]any); row["title"] != "auth flow" {
		t.Fatalf("deep-path query row, want the inserted title: %v", rows[0])
	}

	data = h.mustHTTP("describe_table", map[string]any{"namespace": " ACME / Prod ", "table": "findings"})
	tbl, _ = data["table"].(map[string]any)
	if tbl["namespace"] != "acme/prod" {
		t.Fatalf("normalized describe_table reports the canonical path, got %v", tbl["namespace"])
	}
	if rc, _ := data["row_count"].(float64); rc != 1 {
		t.Fatalf("normalized describe_table reaches the same table, want 1 row: %v", data)
	}
	sc = h.mustMCP("list_tables", map[string]any{"namespace": "ACME/prod"})
	assertJSONEqual(t, "normalized MCP list_tables", sc["tables"], []any{"findings"})

	data = h.mustHTTP("list_namespaces", map[string]any{"prefix": "acme"})
	assertJSONEqual(t, "deep-path prefix listing", data["namespaces"], []any{"acme/prod"})

	status, body := h.httpCall("create_table", map[string]any{
		"namespace": "a/b/c/d",
		"table":     "t",
		"fields":    []map[string]any{{"name": "x", "type": "string"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("over-depth namespace: status %d, want 400: %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid_request" {
		t.Fatalf("over-depth namespace: expected an invalid_request envelope, got %v", body)
	}
	msg, _ := errObj["message"].(string)
	wantMessage(t, "over-depth namespace names the depth rule", msg, `1-3 segments`)

	status, body = h.httpCall("create_namespace", map[string]any{"namespace": " A / / B "})
	if status != http.StatusBadRequest {
		t.Fatalf("empty-segment namespace: status %d, want 400: %v", status, body)
	}
	errObj, _ = body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid_request" {
		t.Fatalf("empty-segment namespace: expected an invalid_request envelope, got %v", body)
	}
	msg, _ = errObj["message"].(string)
	wantMessage(t, "empty-segment namespace names the rule", msg, `no empty segments`)

	h.ensureNS("acme")
	data = h.mustHTTP("list_namespaces", map[string]any{"prefix": "acme"})
	assertJSONEqual(t, "parent exists alongside child", data["namespaces"], []any{"acme", "acme/prod"})
	status, body = h.httpCall("drop_namespace", map[string]any{"namespace": "acme", "confirm": "acme"})
	if status != http.StatusBadRequest {
		t.Fatalf("parent drop with live child: status %d, want 400: %v", status, body)
	}
	errObj, _ = body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid_request" {
		t.Fatalf("parent drop with live child: expected an invalid_request envelope, got %v", body)
	}
	msg, _ = errObj["message"].(string)
	wantMessage(t, "parent drop names its 1 descendant", msg, `namespace acme has 1 descendant namespace`)
	h.mustHTTP("drop_namespace", map[string]any{"namespace": "acme/prod", "confirm": " ACME/PROD "})
	h.mustHTTP("drop_namespace", map[string]any{"namespace": "acme", "confirm": "acme"})
	data = h.mustHTTP("list_namespaces", map[string]any{})
	assertJSONEqual(t, "post-drop listing", data["namespaces"], []any{})
}
