package conformance

import (
	"net/http"
	"reflect"
	"testing"
)

func importBody(key string, records ...map[string]any) map[string]any {
	return map[string]any{"namespace": "imp", "table": "events", "idempotency_key": key, "records": records}
}

func importRecord(id any, body string) map[string]any {
	return map[string]any{"id": id, "created_at": "2026-01-02T03:04:05.123Z", "body": body}
}

func errorCode(out map[string]any) string {
	env, _ := out["error"].(map[string]any)
	code, _ := env["code"].(string)
	return code
}

func TestImportRecordsOverHTTPAndMCP(t *testing.T) {
	h := newHarness(t)
	h.seedTable("imp", "events", []map[string]any{{"name": "body", "type": "text", "fulltext": true}})

	first := h.mustHTTP("import_records", importBody("snap-a-0001", importRecord(9007199254740993, "granite archive"), importRecord(42, "second")))
	want := map[string]any{"ids": []any{float64(9007199254740993), float64(42)}, "inserted": float64(2), "skipped": float64(0), "replayed": false, "changes": float64(2)}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("http import: %v", first)
	}
	rows := h.mustHTTP("read_rows", map[string]any{"namespace": "imp", "table": "events", "ids": []any{42}})["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["created_at"] != "2026-01-02T03:04:05.123Z" {
		t.Fatalf("imported identity: %v", rows)
	}
	found := h.mustHTTP("search_fulltext", map[string]any{"namespace": "imp", "table": "events", "query": "granite"})["results"].([]any)
	if len(found) != 1 {
		t.Fatalf("search over imported row: %v", found)
	}

	replay := h.mustMCP("import_records", importBody("snap-a-0001", importRecord(9007199254740993, "granite archive"), importRecord(42, "second")))
	if replay["replayed"] != true || replay["inserted"] != float64(2) {
		t.Fatalf("mcp replay: %v", replay)
	}
	resumed := h.mustMCP("import_records", importBody("snap-a-regrouped", importRecord(42, "second"), importRecord(43, "third")))
	if resumed["skipped"] != float64(1) || resumed["inserted"] != float64(1) || resumed["changes"] != float64(1) {
		t.Fatalf("mcp resume: %v", resumed)
	}

	status, out := h.httpCall("import_records", importBody("snap-a-0002", importRecord(44, "new"), importRecord(42, "changed")))
	if status != http.StatusBadRequest || errorCode(out) != "conflict" {
		t.Fatalf("differing content: %d %v", status, out)
	}
	if res := h.mcpCall("import_records", importBody("snap-a-0003", importRecord(0, "zero"))); !res.isError() {
		t.Fatalf("mcp accepted a nonpositive id: %+v", res)
	}
	status, out = h.httpCall("import_records", map[string]any{"namespace": "imp", "table": "events", "idempotency_key": "x", "records": []any{map[string]any{"id": 45, "created_at": "2026-01-02T03:04:05.123456Z", "body": "micro"}}})
	if status != http.StatusBadRequest || errorCode(out) != "invalid_request" {
		t.Fatalf("noncanonical timestamp: %d %v", status, out)
	}
	status, out = h.httpCall("import_records", map[string]any{"namespace": "imp", "table": "events", "records": []any{importRecord(46, "keyless")}})
	if status != http.StatusBadRequest || errorCode(out) != "invalid_request" {
		t.Fatalf("missing idempotency key: %d %v", status, out)
	}
	status, out = h.httpCall("import_records", map[string]any{"namespace": "absent", "table": "events", "idempotency_key": "x", "records": []any{importRecord(47, "nowhere")}})
	if status != http.StatusNotFound {
		t.Fatalf("import must not create a namespace: %d %v", status, out)
	}

	next := h.mustHTTP("insert", map[string]any{"namespace": "imp", "table": "events", "records": []any{map[string]any{"body": "ordinary"}}})
	if id := next["ids"].([]any)[0].(float64); id <= 9007199254740993 {
		t.Fatalf("ordinary insert below the imported high-water mark: %v", next)
	}
}

func TestImportRecordsNeedsTableAdminAndCreate(t *testing.T) {
	h := seedGrantHarness(t)
	body := `{"namespace":"acme","table":"docs","idempotency_key":"k1","records":[{"id":7,"created_at":"2026-01-02T03:04:05.123Z","title":"x"}]}`
	grantTo(t, h, "principal", "alice", "acme", "docs", "create")
	res, out := h.asAlice(t, "import_records", body)
	assertForbiddenEnvelope(t, "import with create only", res, out)
	grantTo(t, h, "principal", "alice", "acme", "docs", "admin")
	res, out = h.asAlice(t, "import_records", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("import with admin and create: %d %v", res.StatusCode, out)
	}
}
