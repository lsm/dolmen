package conformance

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestBatchBudgetMessageIsActionableForALargeDelete(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	mk := func(n, from int) []map[string]any {
		recs := make([]map[string]any, n)
		for i := range recs {
			recs[i] = map[string]any{"title": fmt.Sprintf("t%d", from+i), "body": "x"}
		}
		return recs
	}
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "docs", "records": mk(1000, 0)})
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "docs", "records": mk(1, 1000)})

	body := `{"namespace":"acme","confirm":true,"writes":[{"kind":"delete","table":"docs","filter":"1=1"}]}`
	status, out := h.httpCall("batch", json.RawMessage([]byte(body)))
	if status == 200 {
		t.Fatalf("a batch delete over the budget should be refused: %v", out)
	}
	errEnv, _ := out["error"].(map[string]any)
	code, _ := errEnv["code"].(string)
	msg, _ := errEnv["message"].(string)
	if code != "invalid_request" {
		t.Fatalf("code %q, want invalid_request: %v", code, out)
	}
	if strings.Contains(msg, "one insert") {
		t.Fatalf("budget message still says \"one insert\": %q", msg)
	}
	if !strings.Contains(msg, "per-batch budget") || !strings.Contains(msg, "its own delete") {
		t.Fatalf("budget message must describe the budget and the way through: %q", msg)
	}

	left := h.mustHTTP("query", map[string]any{"namespace": "acme", "sql": "SELECT count(*) AS n FROM docs"})
	rows, _ := left["rows"].([]any)
	if n := int64val(t, "n", rows[0].(map[string]any)["n"]); n != 1001 {
		t.Fatalf("the refused batch deleted rows: %d left, want 1001", n)
	}
}
