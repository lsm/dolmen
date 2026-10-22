package conformance

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBatchPrefixesAQueryErrorWithItsWriteIndex(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	body := `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"a","body":"b"}]},
		{"kind":"delete","table":"docs","filter":"nofield = 1"}
	]}`
	status, out := h.httpCall("batch", json.RawMessage([]byte(body)))
	if status == 200 {
		t.Fatalf("the batch should have failed: %v", out)
	}
	errEnv, _ := out["error"].(map[string]any)
	code, _ := errEnv["code"].(string)
	msg, _ := errEnv["message"].(string)
	if code != "query_error" {
		t.Fatalf("code %q, want query_error: %v", code, out)
	}
	if !strings.Contains(msg, "writes[1]:") {
		t.Fatalf("a query_error inside a batch must name the failing write; got %q", msg)
	}
	res := h.mcpCall("batch", json.RawMessage([]byte(body)))
	mcpErr := res.toolError()
	if mcpErr == nil || !strings.Contains(mcpErr["message"].(string), "writes[1]:") {
		t.Fatalf("MCP must prefix the same way: %v", mcpErr)
	}
}
