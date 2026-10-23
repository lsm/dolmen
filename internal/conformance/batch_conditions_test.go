package conformance

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

func TestBatchExpectedRowsEngineAdmission(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	w := store.BatchWrite{Kind: store.BatchWriteDelete, Table: "docs", Filter: "title = 'absent'"}
	if err := json.Unmarshal([]byte(`{"ExpectedRows":1}`), &w); err != nil {
		t.Fatal(err)
	}
	_, err := h.st.Batch(t.Context(), "acme", []store.BatchWrite{
		{Kind: store.BatchWriteInsert, Table: "docs", Records: []map[string]any{{"title": "must-rollback"}}}, w,
	}, store.BatchOpts{}, store.Embedder{}, nil, store.Incarnation{})
	var want error = derr.ErrConflict
	if testEngine(t) != store.EngineSQLite {
		want = store.ErrInvalid
	}
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "expected_rows") {
		t.Fatalf("engine condition admission = %v, want %v", err, want)
	}
	out := h.mustHTTP("query", map[string]any{"namespace": "acme", "sql": "SELECT count(*) AS n FROM docs"})
	if out["rows"].([]any)[0].(map[string]any)["n"] != float64(0) {
		t.Fatalf("rejected engine condition leaked writes: %+v", out)
	}
}

func TestBatchExpectedRowsNotYetExposedOnWire(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	body := map[string]any{"namespace": "acme", "writes": []any{
		map[string]any{"kind": "insert", "table": "docs", "records": []any{map[string]any{"title": "must-rollback"}}},
		map[string]any{"kind": "delete", "table": "docs", "filter": "1=1", "expected_rows": 0},
	}}
	status, out := h.httpCall("batch", body)
	if status != http.StatusBadRequest || !strings.Contains(out["error"].(map[string]any)["message"].(string), "expected_rows") {
		t.Fatalf("wire admitted hidden engine feature: %d %+v", status, out)
	}
	result := h.mcpCall("batch", body)
	if !result.isError() || !strings.Contains(result.toolError()["message"].(string), "expected_rows") {
		t.Fatalf("MCP admitted hidden engine feature: %+v", result)
	}
	rows := h.mustHTTP("query", map[string]any{"namespace": "acme", "sql": "SELECT count(*) AS n FROM docs"})
	if rows["rows"].([]any)[0].(map[string]any)["n"] != float64(0) {
		t.Fatalf("rejected wire request wrote rows: %+v", rows)
	}
}
