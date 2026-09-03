package api

import (
	"strings"
	"testing"
)

func TestNamespaceLifecycleOverHTTP(t *testing.T) {
	srv := newTestServer(t)

	code, res := post(t, srv.URL, "list_namespaces", map[string]any{})
	if code != 200 {
		t.Fatalf("list_namespaces failed: %d %v", code, res)
	}
	if got := res["data"].(map[string]any)["namespaces"]; len(got.([]any)) != 0 {
		t.Fatalf("fresh server must list no namespaces, got %v", got)
	}

	code, res = post(t, srv.URL, "create_namespace", map[string]any{"namespace": "myapp"})
	if code != 200 || res["data"].(map[string]any)["namespace"] != "myapp" {
		t.Fatalf("create_namespace failed: %d %v", code, res)
	}
	code, _ = post(t, srv.URL, "create_namespace", map[string]any{"namespace": "myapp"})
	if code != 400 {
		t.Fatalf("duplicate create_namespace must 400, got %d", code)
	}
	code, _ = post(t, srv.URL, "create_namespace", map[string]any{"namespace": "../escape"})
	if code != 400 {
		t.Fatalf("invalid namespace must 400, got %d", code)
	}

	code, _ = post(t, srv.URL, "create_table", map[string]any{
		"namespace": "myapp",
		"table":     "events",
		"fields":    []map[string]any{{"name": "title", "type": "string", "fulltext": true}},
	})
	if code != 200 {
		t.Fatal("create_table failed")
	}

	code, res = post(t, srv.URL, "list_namespaces", map[string]any{})
	if code != 200 {
		t.Fatalf("list_namespaces failed: %d %v", code, res)
	}
	nss := res["data"].(map[string]any)["namespaces"].([]any)
	if len(nss) != 1 || nss[0] != "myapp" {
		t.Fatalf("expected [myapp], got %v", nss)
	}

	// The drop guard: confirm must repeat the exact namespace name.
	code, _ = post(t, srv.URL, "drop_namespace", map[string]any{"namespace": "myapp"})
	if code != 400 {
		t.Fatalf("drop_namespace without confirm must 400, got %d", code)
	}
	code, res = post(t, srv.URL, "drop_namespace", map[string]any{"namespace": "myapp", "confirm": "other"})
	if code != 400 {
		t.Fatalf("drop_namespace with wrong confirm must 400, got %d", code)
	}
	errEnv, _ := res["error"].(map[string]any)
	msg, _ := errEnv["message"].(string)
	if !strings.Contains(msg, `"myapp"`) {
		t.Fatalf("confirm error must name the required namespace, got: %v", res["error"])
	}

	code, res = post(t, srv.URL, "drop_namespace", map[string]any{"namespace": "myapp", "confirm": "myapp"})
	if code != 200 || res["data"].(map[string]any)["dropped"] != "myapp" {
		t.Fatalf("drop_namespace failed: %d %v", code, res)
	}

	code, res = post(t, srv.URL, "list_namespaces", map[string]any{})
	if code != 200 {
		t.Fatalf("list_namespaces failed: %d %v", code, res)
	}
	if nss := res["data"].(map[string]any)["namespaces"].([]any); len(nss) != 0 {
		t.Fatalf("namespace must be gone after drop, got %v", nss)
	}

	// The dropped namespace is queryable as empty and reusable; dropping a
	// missing one 404s.
	code, _ = post(t, srv.URL, "list_tables", map[string]any{"namespace": "myapp"})
	if code != 200 {
		t.Fatalf("list_tables on the recreated-empty namespace must succeed, got %d", code)
	}
	code, _ = post(t, srv.URL, "drop_namespace", map[string]any{"namespace": "gone", "confirm": "gone"})
	if code != 404 {
		t.Fatalf("drop of missing namespace must 404, got %d", code)
	}
}

func TestDropTableOverHTTP(t *testing.T) {
	srv := newTestServer(t)

	code, _ := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "ns1",
		"table":     "events",
		"fields":    []map[string]any{{"name": "title", "type": "string", "fulltext": true}},
	})
	if code != 200 {
		t.Fatal("create_table failed")
	}
	code, _ = post(t, srv.URL, "insert", map[string]any{
		"namespace":       "ns1",
		"table":           "events",
		"records":         []map[string]any{{"title": "bug"}},
		"idempotency_key": "op-9",
	})
	if code != 200 {
		t.Fatal("insert failed")
	}

	// The drop guard: confirm must repeat the exact table name.
	code, _ = post(t, srv.URL, "drop_table", map[string]any{"namespace": "ns1", "table": "events"})
	if code != 400 {
		t.Fatalf("drop_table without confirm must 400, got %d", code)
	}
	code, _ = post(t, srv.URL, "drop_table", map[string]any{"namespace": "ns1", "table": "events", "confirm": "ns1.events"})
	if code != 400 {
		t.Fatalf("drop_table with wrong confirm must 400, got %d", code)
	}
	code, _ = post(t, srv.URL, "drop_table", map[string]any{"namespace": "ns1", "table": "missing", "confirm": "missing"})
	if code != 404 {
		t.Fatalf("drop of missing table must 404, got %d", code)
	}

	code, res := post(t, srv.URL, "drop_table", map[string]any{"namespace": "ns1", "table": "events", "confirm": "events"})
	if code != 200 || res["data"].(map[string]any)["dropped"] != "events" {
		t.Fatalf("drop_table failed: %d %v", code, res)
	}

	code, res = post(t, srv.URL, "list_tables", map[string]any{"namespace": "ns1"})
	if code != 200 {
		t.Fatalf("list_tables failed: %d %v", code, res)
	}
	if tables := res["data"].(map[string]any)["tables"].([]any); len(tables) != 0 {
		t.Fatalf("table must be gone after drop, got %v", tables)
	}
	code, _ = post(t, srv.URL, "describe_table", map[string]any{"namespace": "ns1", "table": "events"})
	if code != 404 {
		t.Fatalf("describe_table on dropped table must 404, got %d", code)
	}
	code, _ = post(t, srv.URL, "search_fulltext", map[string]any{"namespace": "ns1", "table": "events", "query": "bug"})
	if code != 404 {
		t.Fatalf("search_fulltext on dropped table must 404, got %d", code)
	}

	// Recreating the name starts fresh — in particular the old idempotency
	// key inserts instead of replaying ids that no longer exist.
	code, _ = post(t, srv.URL, "create_table", map[string]any{
		"namespace": "ns1",
		"table":     "events",
		"fields":    []map[string]any{{"name": "title", "type": "string"}},
	})
	if code != 200 {
		t.Fatalf("recreate failed: %d", code)
	}
	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace":       "ns1",
		"table":           "events",
		"records":         []map[string]any{{"title": "fresh"}},
		"idempotency_key": "op-9",
	})
	if code != 200 {
		t.Fatalf("insert with the old key must succeed on the recreated table: %d %v", code, res)
	}
	data := res["data"].(map[string]any)
	if replayed, ok := data["replayed"]; ok && replayed == true {
		t.Fatalf("old idempotency key must not replay on a recreated table, got %v", data)
	}
	if data["inserted"].(float64) != 1 {
		t.Fatalf("expected one fresh insert, got %v", data)
	}
}
