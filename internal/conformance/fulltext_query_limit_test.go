package conformance

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

func TestFulltextQueryOverTheByteLimitIsRefusedFast(t *testing.T) {
	h := newHarness(t)
	h.seedTable("ftl", "n", []map[string]any{{"name": "body", "type": "text", "fulltext": true}})
	h.mustHTTP("insert", map[string]any{"namespace": "ftl", "table": "n", "records": []map[string]any{{"body": "refund issued"}}})

	at := strings.Repeat("a", store.MaxFulltextQueryBytes-len("refund "))
	h.mustHTTP("search_fulltext", map[string]any{"namespace": "ftl", "table": "n", "query": "refund " + at})

	huge := strings.Repeat("refund ", 150000)
	start := time.Now()
	status, out := h.httpCall("search_fulltext", map[string]any{"namespace": "ftl", "table": "n", "query": huge})
	errEnv, _ := out["error"].(map[string]any)
	if status != http.StatusBadRequest || errEnv["code"] != "invalid_request" {
		t.Fatalf("a %d-byte query: status %d %v, want 400 invalid_request", len(huge), status, out)
	}
	if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "byte limit") {
		t.Fatalf("the refusal must name the limit: %q", msg)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("refusing an oversized query took %v; it must be refused before any work", took)
	}
	h.mustHTTP("insert", map[string]any{"namespace": "ftl", "table": "n", "records": []map[string]any{{"body": "still writable"}}})
}
