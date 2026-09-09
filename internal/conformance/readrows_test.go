package conformance

import (
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

// TestReadRowsContract pins the read_rows half of slice 5a: id-addressed
// fetch through both transports with the §2 semantics — found rows once, in
// ascending id order; missing ids simply absent, never an error.
func TestReadRowsContract(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rr", "docs", []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "flag", "type": "boolean"},
		{"name": "meta", "type": "json"},
	})
	ins := h.mustHTTP("insert", map[string]any{
		"namespace": "rr", "table": "docs",
		"records": []map[string]any{
			{"title": "first", "flag": true, "meta": map[string]any{"k": 1}},
			{"title": "second", "flag": false},
			{"title": "third", "flag": true},
		},
	})
	gotIDs, ok := ins["ids"].([]any)
	if !ok || len(gotIDs) != 3 {
		t.Fatalf("insert did not return three ids: %v", ins)
	}

	// Unsorted request with a missing id interleaved: rows come back in
	// ascending id order, the missing id is simply absent, and row_count
	// counts only what returned. The same call over MCP must answer
	// identically — the parity the shared Dispatch path owes every op.
	body := map[string]any{"namespace": "rr", "table": "docs", "ids": []any{2.0, 9999.0, 1.0}}
	data := h.mustHTTP("read_rows", body)
	assertJSONEqual(t, "read_rows over MCP vs HTTP", h.mustMCP("read_rows", body), data)

	rows, ok := data["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected the two found rows, got %v", data)
	}
	if int64val(t, "row_count", data["row_count"]) != 2 {
		t.Fatalf("row_count must count the returned rows: %v", data)
	}
	first, _ := rows[0].(map[string]any)
	second, _ := rows[1].(map[string]any)
	if int64val(t, "rows[0].id", first["id"]) != 1 || int64val(t, "rows[1].id", second["id"]) != 2 {
		t.Fatalf("rows must be in ascending id order regardless of request order: %v", rows)
	}
	if first["title"] != "first" || first["flag"] != true {
		t.Fatalf("rows must carry their typed fields: %v", first)
	}
	if meta, ok := first["meta"].(map[string]any); !ok || meta["k"] != float64(1) {
		t.Fatalf("json field must decode as the object stored, got %T %v", first["meta"], first["meta"])
	}
	if data["truncated"] != false {
		t.Fatalf("truncated must be present and false for a complete page, got %v", data["truncated"])
	}

	// Empty id set: a well-formed no-op — an empty rows array, never null.
	empty := h.mustHTTP("read_rows", map[string]any{"namespace": "rr", "table": "docs", "ids": []any{}})
	if rows, ok := empty["rows"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("empty ids must return an empty rows array, got %v", empty)
	}
	if int64val(t, "row_count", empty["row_count"]) != 0 {
		t.Fatalf("row_count must be 0 for an empty id set: %v", empty)
	}

	// Absence-not-error does not stop at ids: a missing table is still
	// not_found, the same error the other table reads report.
	status, out := h.httpCall("read_rows", map[string]any{"namespace": "rr", "table": "nope", "ids": []any{1}})
	if status != 404 {
		t.Fatalf("read_rows on a missing table must be 404, got %d %v", status, out)
	}
}

// TestReadRowsIDCapEnforced pins §2's cap at the transport boundary: at most
// store.MaxReadRowsIDs ids per request — invalid_request beyond, over both
// transports, with the same envelope (the error-parity table repeats the
// shape; this pins the boundary on both sides).
func TestReadRowsIDCapEnforced(t *testing.T) {
	h := newHarness(t)
	h.seedTable("cap", "t", []map[string]any{{"name": "title", "type": "string"}})

	atCap := make([]any, store.MaxReadRowsIDs)
	for i := range atCap {
		atCap[i] = float64(i + 1)
	}
	data := h.mustHTTP("read_rows", map[string]any{"namespace": "cap", "table": "t", "ids": atCap})
	if rows, ok := data["rows"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("a capped request over an empty table returns an empty page, got %v", data)
	}

	over := append(atCap, float64(store.MaxReadRowsIDs+1))
	status, out := h.httpCall("read_rows", map[string]any{"namespace": "cap", "table": "t", "ids": over})
	if status != 400 {
		t.Fatalf("read_rows beyond the id cap must be 400, got %d %v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "invalid_request" {
		t.Fatalf("read_rows beyond the id cap must be invalid_request, got %v", errObj)
	}
	mcpRes := h.mcpCall("read_rows", map[string]any{"namespace": "cap", "table": "t", "ids": over})
	if !mcpRes.isError() {
		t.Fatalf("read_rows beyond the id cap must fail over MCP too, got %+v", mcpRes)
	}
	assertJSONEqual(t, "id-cap error envelope", withoutRequestID(mcpRes.toolError()), withoutRequestID(errObj))
}

// TestCapabilitiesShapePinned pins the capabilities op (§2, §6.2): the
// engine's self-description serialized verbatim — the four pinned fields,
// exact and ann_recall_bound explicitly null for adapter #1, over both
// transports byte-identical.
func TestCapabilitiesShapePinned(t *testing.T) {
	h := newHarness(t)

	want := map[string]any{
		"vector_execution": "exact",
		"ann_recall_bound": nil,
		"notifications":    false,
		"subscribe":        false,
	}
	data := h.mustHTTP("capabilities", map[string]any{})
	assertJSONEqual(t, "capabilities", data, want)
	assertJSONEqual(t, "capabilities over MCP vs HTTP", h.mustMCP("capabilities", map[string]any{}), data)

	// ann_recall_bound is explicit null — present as a key — never omitted:
	// decode into a map that distinguishes the two.
	if _, present := data["ann_recall_bound"]; !present {
		t.Fatalf("ann_recall_bound must be present (explicit null under exact execution), got %v", data)
	}
}
