package conformance

import (
	"net/http"
	"testing"
)

// TestNamespaceCreatedOnFirstUse pins the auth:off namespace contract v0.2.0
// served through the store's implicit create-on-open and slice 2c restored
// through the op layer's explicit ensureNamespace: an operation naming a
// namespace that does not exist behaves exactly as it did when the store
// materialized the namespace as a side effect — the namespace exists
// afterwards, reads over it return empty results (not not_found), and the
// failure, when there is one, names the table, never the namespace. The
// engine itself never creates implicitly (§6.2); the op layer, which knows
// auth is off, does the ensuring. 7e makes this mode-dependent.
func TestNamespaceCreatedOnFirstUse(t *testing.T) {
	h := newHarness(t)

	// A write into a never-created namespace: v0.2.0 opened — creating — the
	// namespace, then failed on the missing table. The error names the table.
	status, body := h.httpCall("insert", map[string]any{
		"namespace": "firstuse", "table": "t",
		"records":   []map[string]any{{"a": "x"}},
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

	// The namespace now exists: first use materialized it, as v0.2.0 did —
	// through an explicit CreateNamespace, not a store side effect.
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

	// Reads over the freshly materialized namespace are empty, not not_found.
	tables := h.mustHTTP("list_tables", map[string]any{"namespace": "firstuse"})
	if ts, _ := tables["tables"].([]any); len(ts) != 0 {
		t.Fatalf("a freshly created namespace lists no tables: %v", ts)
	}
}
