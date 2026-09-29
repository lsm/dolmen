package conformance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func batchDocs(t *testing.T, h *harness) {
	t.Helper()
	h.seedTable("acme", "docs", []map[string]any{{"name": "title"}, {"name": "body", "type": "text"}})
}

func batchData(t *testing.T, h *harness, body string) map[string]any {
	t.Helper()
	status, out := h.httpCall("batch", json.RawMessage([]byte(body)))
	if status != http.StatusOK {
		t.Fatalf("/v1/batch failed: status %d %v", status, out)
	}
	data, ok := out["data"].(map[string]any)
	if !ok {
		t.Fatalf("/v1/batch returned no data object: %v", out)
	}
	return data
}

func batchResults(t *testing.T, h *harness, body string) []any {
	t.Helper()
	data := batchData(t, h, body)
	if replayed, _ := data["replayed"].(bool); replayed {
		t.Fatalf("a batch with no idempotency key reported a replay: %v", data)
	}
	results, ok := data["results"].([]any)
	if !ok {
		t.Fatalf("batch returned no results array: %v", data)
	}
	return results
}

func batchError(t *testing.T, h *harness, body string) (string, string) {
	t.Helper()
	status, out := h.httpCall("batch", json.RawMessage([]byte(body)))
	if status == http.StatusOK {
		t.Fatalf("the batch was accepted but should not have been: %v", out)
	}
	errEnv, _ := out["error"].(map[string]any)
	msg, _ := errEnv["message"].(string)
	code, _ := errEnv["code"].(string)
	return code, msg
}

func TestBatchOverHTTPCommitsEveryWriteInOrder(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)

	results := batchResults(t, h, `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"one","body":"first"}]},
		{"kind":"insert","table":"docs","records":[{"title":"two","body":"second"}]},
		{"kind":"update","table":"docs","filter":"title = 'one'","set":{"body":"edited"}}
	]}`)

	if len(results) != 3 {
		t.Fatalf("got %d results, want one per write", len(results))
	}
	first, _ := results[0].(map[string]any)
	if inserted, _ := first["inserted"].(float64); inserted != 1 {
		t.Fatalf("insert result = %v, want inserted 1", first)
	}
	if _, ok := first["ids"]; !ok {
		t.Fatalf("insert result carries no ids: %v", first)
	}
	third, _ := results[2].(map[string]any)
	if updated, _ := third["updated"].(float64); updated != 1 {
		t.Fatalf("update result = %v, want updated 1", third)
	}
	if _, ok := third["inserted"]; ok {
		t.Fatalf("an update result carries insert fields, so the per-kind schema is not derived: %v", third)
	}
}

func TestBatchOverHTTPNamesTheFailingWriteAndCommitsNothing(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)

	code, msg := batchError(t, h, `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"kept","body":"valid"}]},
		{"kind":"insert","table":"docs","records":[{"titel":"typo","body":"invalid"}]}
	]}`)
	if !strings.Contains(msg, "writes[1]") {
		t.Fatalf("error %q does not name the failing write by index", msg)
	}
	if code != "invalid_request" {
		t.Fatalf("error code %q, want the failing write's own class invalid_request", code)
	}

	rows := h.mustHTTP("query", json.RawMessage([]byte(`{"namespace":"acme","sql":"SELECT count(*) AS n FROM docs"}`)))
	list, _ := rows["rows"].([]any)
	if len(list) != 1 {
		t.Fatalf("count query returned %v", list)
	}
	row, _ := list[0].(map[string]any)
	if n, _ := row["n"].(float64); n != 0 {
		t.Fatalf("the failed batch left %v rows, want none", row)
	}
}

func TestBatchOverHTTPPublishesTheWholeFeedOnce(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	head := nextCursorOf(t, h.mustHTTP("changes_since", json.RawMessage([]byte(`{"namespace":"acme"}`))))

	batchResults(t, h, `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"one","body":"a"},{"title":"two","body":"b"}]},
		{"kind":"update","table":"docs","filter":"title = 'one'","set":{"body":"edited"}}
	]}`)

	records := changesOf(t, h.mustHTTP("changes_since", json.RawMessage([]byte(`{"namespace":"acme","cursor":"`+head+`"}`))))
	if len(records) != 3 {
		t.Fatalf("the batch published %d change records, want 3 in one commit", len(records))
	}
	kinds := map[string]int{}
	for _, r := range records {
		kinds[r[2].(string)]++
	}
	if kinds["insert"] != 2 || kinds["update"] != 1 {
		t.Fatalf("change kinds = %v, want 2 inserts and 1 update", kinds)
	}
}

func TestBatchOverHTTPReplaysWithItsKeyAndRefusesAPerWriteKey(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	body := `{"namespace":"acme","idempotency_key":"import-1","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"once","body":"only ever once"}]}
	]}`

	first := batchData(t, h, body)
	if replayed, _ := first["replayed"].(bool); replayed {
		t.Fatalf("the first batch reported a replay")
	}
	second := batchData(t, h, body)
	if replayed, _ := second["replayed"].(bool); !replayed {
		t.Fatalf("the replay did not report itself as one: %v", second)
	}
	firstResults, _ := first["results"].([]any)
	secondResults, _ := second["results"].([]any)
	firstIDs, _ := firstResults[0].(map[string]any)["ids"].([]any)
	secondIDs, _ := secondResults[0].(map[string]any)["ids"].([]any)
	if len(firstIDs) != 1 || len(secondIDs) != 1 || firstIDs[0] != secondIDs[0] {
		t.Fatalf("replay returned %v, want the stored ids %v", secondIDs, firstIDs)
	}

	_, msg := batchError(t, h, `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","idempotency_key":"inner","records":[{"title":"x","body":"y"}]}
	]}`)
	if !strings.Contains(msg, "writes[0]") {
		t.Fatalf("error %q does not name the write carrying its own key", msg)
	}
}

func TestBatchOverHTTPRefusesAPerWriteDryRun(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)

	_, msg := batchError(t, h, `{"namespace":"acme","writes":[
		{"kind":"delete","table":"docs","filter":"1=1","dry_run":true}
	]}`)
	if !strings.Contains(msg, "writes[0]") {
		t.Fatalf("error %q does not name the write carrying dry_run", msg)
	}
}

func TestBatchOverMCPReportsPerWriteResults(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)

	sc := h.mustMCP("batch", map[string]any{
		"namespace": "acme",
		"writes": []any{
			map[string]any{"kind": "insert", "table": "docs", "records": []any{map[string]any{"title": "one", "body": "first"}}},
			map[string]any{"kind": "update", "table": "docs", "filter": "title = 'one'", "set": map[string]any{"body": "edited"}},
		},
	})
	results, ok := sc["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("MCP batch returned %v, want two results", sc)
	}
}
