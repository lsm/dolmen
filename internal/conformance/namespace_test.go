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

// TestNamespaceTreeListingAndLeafOnlyDrops pins slice 3b's tree contract on
// the wire: list_namespaces reports the whole nested tree sorted by full
// path (no prefix — the v0.2.0 request, answered with v0.2.0's response
// shape), the additive prefix (§5.3) filters to the prefix's subtree, and
// drop_namespace refuses a namespace with descendants (§5.4) naming the
// count — child-first drops succeed down to the leaf.
func TestNamespaceTreeListingAndLeafOnlyDrops(t *testing.T) {
	h := newHarness(t)

	for _, ns := range []string{"acme", "acme/prod", "acme/prod/eu", "acme/stage", "acme2", "zeta"} {
		h.ensureNS(ns)
	}

	// No prefix (the v0.2.0 request, byte-identical in shape): every
	// namespace depth 1–3, sorted lexicographically by full path.
	data := h.mustHTTP("list_namespaces", map[string]any{})
	assertJSONEqual(t, "full recursive listing", data["namespaces"],
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage", "acme2", "zeta"})

	// Prefix: the recursive subtree, the prefix itself included, and
	// stem-siblings (acme2) excluded.
	data = h.mustHTTP("list_namespaces", map[string]any{"prefix": "acme"})
	assertJSONEqual(t, "prefix listing", data["namespaces"],
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage"})
	data = h.mustHTTP("list_namespaces", map[string]any{"prefix": "acme/prod"})
	assertJSONEqual(t, "deep prefix listing", data["namespaces"],
		[]any{"acme/prod", "acme/prod/eu"})
	// A prefix is a namespace input and normalizes like one: same listing
	// through MCP's structuredContent as over HTTP.
	sc := h.mustMCP("list_namespaces", map[string]any{"prefix": " ACME "})
	assertJSONEqual(t, "normalized MCP prefix listing", sc["namespaces"],
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage"})
	data = h.mustHTTP("list_namespaces", map[string]any{"prefix": "ghost"})
	assertJSONEqual(t, "absent prefix listing", data["namespaces"], []any{})

	// An invalid prefix is invalid_request naming the path grammar — never
	// a silent empty listing.
	status, body := h.httpCall("list_namespaces", map[string]any{"prefix": "acme/prod/eu/deep"})
	if status != http.StatusBadRequest {
		t.Fatalf("over-depth prefix: status %d, want 400: %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid_request" {
		t.Fatalf("over-depth prefix: expected an invalid_request envelope, got %v", body)
	}

	// §5.4: dropping a namespace with descendants is refused, the message
	// naming the count; nothing was dropped.
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
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage", "acme2", "zeta"})

	// Child-first drops succeed: leaf, then parents, down to the root.
	for _, ns := range []string{"acme/prod/eu", "acme/prod", "acme/stage", "acme"} {
		h.mustHTTP("drop_namespace", map[string]any{"namespace": ns, "confirm": ns})
	}
	data = h.mustHTTP("list_namespaces", map[string]any{})
	assertJSONEqual(t, "post-drop listing", data["namespaces"], []any{"acme2", "zeta"})
}
