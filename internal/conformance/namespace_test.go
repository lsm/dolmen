package conformance

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

func TestReadOpsNeverCreateNamespaceFiles(t *testing.T) {
	h := newHarness(t)

	reads := []struct {
		op   string
		body map[string]any
	}{
		{"list_tables", map[string]any{"namespace": "ghost01"}},
		{"describe_table", map[string]any{"namespace": "ghost02", "table": "t"}},
		{"query", map[string]any{"namespace": "ghost03", "sql": "SELECT 1"}},
		{"search_fulltext", map[string]any{"namespace": "ghost04", "table": "t", "query": "x"}},
		{"search_vector", map[string]any{"namespace": "ghost05", "table": "t", "vector": []float64{1, 2, 3, 4, 5, 6, 7, 8}}},
		{"search_vector", map[string]any{"namespace": "ghost06", "table": "t", "text": "x"}},
		{"read_rows", map[string]any{"namespace": "ghost07", "table": "t", "ids": []int64{1}}},
		{"changes_since", map[string]any{"namespace": "ghost08"}},
		{"wait_for", map[string]any{"namespace": "ghost09", "timeout_ms": 0}},
		{"list_migrations", map[string]any{"namespace": "ghost10", "table": "t"}},
		{"drop_table", map[string]any{"namespace": "ghost11", "table": "t", "confirm": "t"}},
	}

	for _, r := range reads {
		t.Run(r.op+"/"+r.body["namespace"].(string), func(t *testing.T) {
			status, out := h.httpCall(r.op, r.body)
			if status != 404 {
				t.Fatalf("%s on a missing namespace: status %d, want 404: %v", r.op, status, out)
			}
			errObj, _ := out["error"].(map[string]any)
			if errObj == nil || errObj["code"] != "not_found" {
				t.Fatalf("%s on a missing namespace must answer not_found: %v", r.op, out)
			}

			res := h.mcpCall(r.op, r.body)
			if !res.isError() {
				t.Fatalf("MCP %s on a missing namespace must fail: %+v", r.op, res)
			}
			if env := res.toolError(); env["code"] != "not_found" {
				t.Fatalf("MCP %s code %v, want not_found: %v", r.op, env["code"], env)
			}
		})
	}

	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("a read against a missing namespace left %s on disk; discovery must never create files", e.Name())
	}
}

func TestWriteOpsStillCreateNamespacesImplicitly(t *testing.T) {
	h := newHarness(t)

	h.mustHTTP("create_table", map[string]any{
		"namespace": "born01", "table": "notes",
		"fields": []any{map[string]any{"name": "body", "type": "text"}},
	})
	if _, err := os.Stat(filepath.Join(h.dir, "born01.db")); err != nil {
		t.Fatalf("create_table must create its namespace implicitly: %v", err)
	}

	names := h.mustHTTP("list_namespaces", map[string]any{})
	list, _ := names["namespaces"].([]any)
	found := false
	for _, n := range list {
		if n == "born01" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the implicitly created namespace must be listed: %v", names)
	}
}

func TestDropNamespaceNotFoundDoesNotAdviseCreating(t *testing.T) {
	h := newHarness(t)
	status, out := h.httpCall("drop_namespace", map[string]any{"namespace": "nosuchns", "confirm": "nosuchns"})
	if status != 404 {
		t.Fatalf("status %d, want 404: %v", status, out)
	}
	msg := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "nothing was dropped") {
		t.Fatalf("a failed drop must say nothing was dropped, got %q", msg)
	}
	for _, bad := range []string{"create_namespace", "create it on first use"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("a failed drop must not advise recreating the namespace the caller asked to delete, got %q", msg)
		}
	}
}
