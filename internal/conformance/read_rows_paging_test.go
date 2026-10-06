package conformance

import (
	"net/http"
	"testing"
)

func pageIDs(t *testing.T, data map[string]any) []int64 {
	t.Helper()
	rows, _ := data["rows"].([]any)
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		m, _ := r.(map[string]any)
		out = append(out, int64val(t, "id", m["id"]))
	}
	return out
}

func TestReadRowsPagesThroughATableWithoutIDs(t *testing.T) {
	h := newHarness(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "pg"})
	h.seedTable("pg", "deals", []map[string]any{{"name": "title", "type": "string"}})
	records := []map[string]any{}
	for _, title := range []string{"a", "b", "c", "d", "e"} {
		records = append(records, map[string]any{"title": title})
	}
	h.mustHTTP("insert", map[string]any{"namespace": "pg", "table": "deals", "records": records})
	h.mustHTTP("delete", map[string]any{"namespace": "pg", "table": "deals", "filter": "title = 'b'", "confirm": true})

	var seen []int64
	after := int64(0)
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatalf("paging never ended: %v", seen)
		}
		data := h.mustHTTP("read_rows", map[string]any{"namespace": "pg", "table": "deals", "after_id": after, "limit": 2})
		ids := pageIDs(t, data)
		seen = append(seen, ids...)
		if data["truncated"] != true {
			if _, ok := data["next_after_id"]; ok {
				t.Fatalf("the last page must not offer a next_after_id: %v", data)
			}
			break
		}
		after = int64val(t, "next_after_id", data["next_after_id"])
		if after != ids[len(ids)-1] {
			t.Fatalf("next_after_id %d must be the last id on the page %v", after, ids)
		}
	}
	if want := []int64{1, 3, 4, 5}; len(seen) != len(want) || seen[0] != 1 || seen[1] != 3 || seen[2] != 4 || seen[3] != 5 {
		t.Fatalf("paging read %v, want %v: every live row once, in id order", seen, want)
	}

	data := h.mustHTTP("read_rows", map[string]any{"namespace": "pg", "table": "deals"})
	if got := pageIDs(t, data); len(got) != 4 || data["truncated"] != false {
		t.Fatalf("read_rows with neither ids nor after_id must read from the start: %v", data)
	}
	for _, bad := range []map[string]any{
		{"namespace": "pg", "table": "deals", "ids": []int{1}, "after_id": 0},
		{"namespace": "pg", "table": "deals", "ids": []int{1}, "limit": 2},
		{"namespace": "pg", "table": "deals", "limit": 0},
		{"namespace": "pg", "table": "deals", "limit": 1001},
		{"namespace": "pg", "table": "deals", "after_id": -1},
	} {
		if status, out := h.httpCall("read_rows", bad); status != http.StatusBadRequest {
			t.Fatalf("read_rows %v: status %d, want 400: %v", bad, status, out)
		}
	}
}

func TestReadRowsPagingKeepsToTheCallersOwnRows(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "notes", "fields": []map[string]any{{"name": "body", "type": "text"}}, "row_access": "own"})
	grantTo(t, h, "group", "staff", "acme", "notes", "create")
	for _, who := range []string{"alice", "bob", "alice"} {
		if res, out := h.asIdentity(t, who, "staff", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"x"}]}`); res.StatusCode != http.StatusOK {
			t.Fatalf("insert as %s: %d %v", who, res.StatusCode, out)
		}
	}
	res, out := h.asIdentity(t, "alice", "staff", "read_rows", `{"namespace":"acme","table":"notes","limit":10}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("read_rows as an own-row writer: %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if ids := pageIDs(t, data); len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Fatalf("an own-row writer must page through only their own rows, got ids %v", ids)
	}
}
