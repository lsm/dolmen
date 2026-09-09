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
// the wire: list_namespaces reports the whole nested tree in database-
// filename order — v0.2.0's os.ReadDir order, byte-identical on a
// depth-1-only store (no prefix — the v0.2.0 request, answered with
// v0.2.0's response shape), the additive prefix (§5.3) filters to the
// prefix's subtree, and drop_namespace refuses a namespace with
// descendants (§5.4) naming the count — child-first drops succeed down to
// the leaf.
func TestNamespaceTreeListingAndLeafOnlyDrops(t *testing.T) {
	h := newHarness(t)

	// edge/edge-x pin the order on the one pair where it visibly matters:
	// v0.2.0's ReadDir ordered edge-x.db before edge.db ('-' < '.').
	for _, ns := range []string{"acme", "acme/prod", "acme/prod/eu", "acme/stage", "acme2", "edge", "edge-x", "zeta"} {
		h.ensureNS(ns)
	}

	// No prefix (the v0.2.0 request, byte-identical in shape): every
	// namespace depth 1–3, in database-filename order.
	data := h.mustHTTP("list_namespaces", map[string]any{})
	assertJSONEqual(t, "full recursive listing", data["namespaces"],
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage", "acme2", "edge-x", "edge", "zeta"})

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

	// A present-but-empty prefix is a request error too, not the omitted
	// field: silently listing everything would mask the caller's own bug.
	// (decode's null probe rejects "prefix": null as every other null.)
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
		[]any{"acme", "acme/prod", "acme/prod/eu", "acme/stage", "acme2", "edge-x", "edge", "zeta"})

	// Child-first drops succeed: leaf, then parents, down to the root.
	for _, ns := range []string{"acme/prod/eu", "acme/prod", "acme/stage", "acme"} {
		h.mustHTTP("drop_namespace", map[string]any{"namespace": ns, "confirm": ns})
	}
	data = h.mustHTTP("list_namespaces", map[string]any{})
	assertJSONEqual(t, "post-drop listing", data["namespaces"], []any{"acme2", "edge-x", "edge", "zeta"})
}

// TestNamespaceDeepPathLifecycle pins slice 3c's API surface: the schema
// surfaces accept the path form and request normalization is per segment
// (§5.1). A table is created under a two-segment namespace through the op
// layer — create_table's implicit ensure makes acme/prod (and the acme/
// directory) without acme ever existing as a namespace — written and read
// back, listed through the prefix, and dropped child-first: the deep-path
// lifecycle end to end. Depth-1 namespaces are untouched by every step
// (the widened patterns still match them — §8.1's additive rule; the rest
// of the suite pins that world byte for byte).
func TestNamespaceDeepPathLifecycle(t *testing.T) {
	h := newHarness(t)

	// Create under a path: the created table reports the path as its
	// namespace, the row count of a fresh table is zero.
	data := h.mustHTTP("create_table", map[string]any{
		"namespace": "acme/prod",
		"table":     "findings",
		"fields":    []map[string]any{{"name": "title", "type": "string"}},
	})
	tbl, _ := data["table"].(map[string]any)
	if tbl["namespace"] != "acme/prod" {
		t.Fatalf("created table reports its namespace path, got %v", tbl["namespace"])
	}

	// Rows go in and come back through the deep path, over both transports.
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

	// Per-segment normalization (§5.1): v0.2.0's trim-and-lowercase, per
	// segment — the padded, cased path names the same namespace over the
	// direct wire, and over MCP the same way (the schemas advertise
	// canonical names; the server still normalizes what it is handed).
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

	// Listing with a prefix reports the deep namespace; acme itself was
	// never created, so the subtree holds only acme/prod.
	data = h.mustHTTP("list_namespaces", map[string]any{"prefix": "acme"})
	assertJSONEqual(t, "deep-path prefix listing", data["namespaces"], []any{"acme/prod"})

	// The grammar still bounds the surface: an over-depth path is
	// invalid_request through the op layer, the validator's message naming
	// the depth rule.
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

	// Normalization never silently repairs an empty segment: " A / / B "
	// canonicalizes per segment to "a//b", which validation rejects. The wire
	// behavior is pinned here because normNS alone round-trips the input — a
	// future cleanup that drops empty segments would otherwise pass every unit
	// test while silently creating a/b here (a and b do not exist yet at this
	// point, so the status, code, and message assertions all carry weight).
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

	// Drop child then parent: creating acme as a namespace of its own
	// leaves the parent refusing to drop over its child; the child drops
	// with confirm carrying the path (normalized like the namespace
	// itself), and the parent then drops cleanly.
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
