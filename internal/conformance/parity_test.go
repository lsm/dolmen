package conformance

import (
	"net/http"
	"testing"

	"github.com/lsm/dolmen/internal/api"
)

// parityScript is one deterministic tour through every operation, in a
// lifecycle order (create → write → read → search → migrate → delete → drop).
// Each step's inputs are identical over both transports; the transport is the
// only variable. Volatile server-assigned timestamps (created_at, migration
// "at") are masked before comparison.
type parityStep struct {
	name string
	op   string
	body map[string]any
	// skipHTTP runs the step only over MCP (for payload variants that must
	// not run twice on the HTTP side); unused by the standard script.
	skipHTTP bool
}

func parityScript() []parityStep {
	ns := "parity"
	fields := []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "tag", "type": "string"},
		{"name": "score", "type": "number"},
		{"name": "flag", "type": "boolean"},
		{"name": "meta", "type": "json"},
		{"name": "vec", "type": "vector", "dim": 4},
	}
	return []parityStep{
		{"create_namespace", "create_namespace", map[string]any{"namespace": ns}, false},
		{"create_table", "create_table", map[string]any{"namespace": ns, "table": "docs", "fields": fields}, false},
		{"list_tables", "list_tables", map[string]any{"namespace": ns}, false},
		{"insert", "insert", map[string]any{
			"namespace": ns, "table": "docs",
			"records": []map[string]any{
				{"title": "first bug", "body": "token expiry not checked", "tag": "a", "score": 0.75, "flag": true, "meta": map[string]any{"k": []any{1, 2}}, "vec": []any{0.5, 0.25, -0.5, 0.0}},
				{"title": "second crash", "body": "payment gateway timeout", "tag": "b", "score": 2, "flag": false, "vec": []any{1, 0, 0, 0}},
			},
		}, false},
		{"insert_idempotent", "insert", map[string]any{
			"namespace": ns, "table": "docs", "idempotency_key": "retry-1",
			"records": []map[string]any{{"title": "idem", "tag": "k", "score": 1}},
		}, false},
		{"insert_replay", "insert", map[string]any{
			"namespace": ns, "table": "docs", "idempotency_key": "retry-1",
			"records": []map[string]any{{"title": "idem", "tag": "k", "score": 1}},
		}, false},
		{"upsert_by_key", "upsert_by_key", map[string]any{
			"namespace": ns, "table": "docs", "on": []string{"tag"},
			"records": []map[string]any{{"tag": "a", "score": 9}},
		}, false},
		{"upsert_insert_branch", "upsert", map[string]any{
			"namespace": ns, "table": "docs", "filter": "tag = 'zz'",
			"set": map[string]any{"tag": "zz", "title": "new row"},
		}, false},
		{"update", "update", map[string]any{
			"namespace": ns, "table": "docs", "filter": "tag = 'zz'",
			"set": map[string]any{"score": 1.5},
		}, false},
		{"query", "query", map[string]any{
			"namespace": ns,
			"sql":       "SELECT id, title, tag, score, flag, meta, vec FROM docs ORDER BY id",
		}, false},
		{"search_fulltext", "search_fulltext", map[string]any{
			"namespace": ns, "table": "docs", "query": "bug OR crash",
		}, false},
		{"search_fulltext_filtered", "search_fulltext", map[string]any{
			"namespace": ns, "table": "docs", "query": "bug OR crash",
			"filter": "score >= ?", "args": []any{1},
		}, false},
		{"search_vector_raw", "search_vector", map[string]any{
			"namespace": ns, "table": "docs", "column": "vec", "vector": []any{0.5, 0.25, -0.5, 0.0},
		}, false},
		{"search_vector_text", "search_vector", map[string]any{
			"namespace": ns, "table": "docs", "text": "payment gateway timeout",
		}, false},
		{"describe_table", "describe_table", map[string]any{"namespace": ns, "table": "docs"}, false},
		{"migrate", "migrate", map[string]any{
			"namespace": ns, "table": "docs", "expected_version": 1,
			"changes": []map[string]any{{
				"op": "add_field", "field": map[string]any{"name": "status", "type": "string"}, "default": "open",
			}},
		}, false},
		{"migrate_dry_run", "migrate", map[string]any{
			"namespace": ns, "table": "docs", "dry_run": true, "expected_version": 2,
			"changes": []map[string]any{{"op": "rename_field", "from": "status", "to": "state"}},
		}, false},
		{"list_migrations", "list_migrations", map[string]any{"namespace": ns, "table": "docs"}, false},
		{"delete_dry_run", "delete", map[string]any{
			"namespace": ns, "table": "docs", "filter": "tag = 'zz'", "dry_run": true,
		}, false},
		{"delete", "delete", map[string]any{
			"namespace": ns, "table": "docs", "filter": "tag = 'zz'",
		}, false},
		{"infer_schema", "infer_schema", map[string]any{
			"samples": []map[string]any{{"a": 1, "b": "x", "c": true}},
		}, false},
		{"drop_table", "drop_table", map[string]any{"namespace": ns, "table": "docs", "confirm": "docs"}, false},
		{"list_namespaces", "list_namespaces", map[string]any{}, false},
		{"describe_server", "describe_server", map[string]any{}, false},
		{"drop_namespace", "drop_namespace", map[string]any{"namespace": ns, "confirm": ns}, false},
	}
}

// TestTransportParityAllOperations drives the identical script against two
// independent servers — one over /v1, one over MCP tools/call — and requires
// every step to produce the identical result: HTTP data == MCP
// structuredContent, modulo server-assigned timestamps. This is the drift
// guard between the two transports (#119 caught them disagreeing once). The
// registry-coverage check below keeps the script at one step per operation as
// the registry grows.
func TestTransportParityAllOperations(t *testing.T) {
	httpH := newHarness(t)
	mcpH := newHarness(t)

	steps := parityScript()

	// The script must exercise every registered operation — a new op added
	// to the registry without a parity step fails here instead of drifting.
	covered := map[string]bool{}
	for _, step := range steps {
		covered[step.op] = true
	}
	for _, name := range api.OpNames() {
		if !covered[name] {
			t.Errorf("parity script never exercises operation %q", name)
		}
	}

	httpData := make([]map[string]any, len(steps))
	mcpData := make([]map[string]any, len(steps))

	for i, step := range steps {
		status, out := httpH.httpCall(step.op, step.body)
		if status != http.StatusOK || out["ok"] != true {
			t.Errorf("step %d (%s over HTTP) failed: status %d %v", i, step.name, status, out)
			return
		}
		data, ok := out["data"].(map[string]any)
		if !ok {
			t.Errorf("step %d (%s over HTTP) returned no data: %v", i, step.name, out)
			return
		}
		httpData[i] = data
	}

	for i, step := range steps {
		res := mcpH.mcpCall(step.op, step.body)
		if res.status != http.StatusOK || res.proto != nil {
			t.Errorf("step %d (%s over MCP) protocol failure: %+v", i, step.name, res)
			return
		}
		if res.isError() {
			t.Errorf("step %d (%s over MCP) tool error: %v", i, step.name, res.toolError())
			return
		}
		sc := res.structured()
		if sc == nil {
			t.Errorf("step %d (%s over MCP) returned no structuredContent", i, step.name)
			return
		}
		mcpData[i] = sc
	}

	// Compare per step as subtests so a drift report names the operation.
	for i, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			assertJSONEqual(t, step.name+" result", maskVolatile(t, mcpData[i]), maskVolatile(t, httpData[i]))
		})
	}
}

// TestTransportParityErrorEnvelope pins the error side of transport parity:
// the same failing call must report the same stable code and message through
// both transports — HTTP as {"ok":false,"error":{...}} with the pinned
// status, MCP as a tool error whose text is exactly that same envelope.
func TestTransportParityErrorEnvelope(t *testing.T) {
	h := newHarness(t)
	h.seedTable("errp", "t", []map[string]any{{"name": "title", "type": "string", "fulltext": true}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "errp", "table": "t", "idempotency_key": "diverge",
		"records": []map[string]any{{"title": "one"}},
	})

	cases := []struct {
		name   string
		op     string
		body   map[string]any
		status int
	}{
		{"missing table", "describe_table", map[string]any{"namespace": "errp", "table": "nope"}, 404},
		{"unknown request field", "list_tables", map[string]any{"namespace": "errp", "extra": 1}, 400},
		{"null option value", "list_tables", map[string]any{"namespace": nil}, 400},
		{"bad sql", "query", map[string]any{"namespace": "errp", "sql": "SELECT nope FROM t"}, 400},
		{"write sql", "query", map[string]any{"namespace": "errp", "sql": "DELETE FROM t"}, 400},
		{"fts syntax error", "search_fulltext", map[string]any{"namespace": "errp", "table": "t", "query": "bug-"}, 400},
		{"unknown field in record", "insert", map[string]any{
			"namespace": "errp", "table": "t",
			"records": []map[string]any{{"bogus": 1}},
		}, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			httpStatus, httpBody := h.httpCall(c.op, c.body)
			mcpRes := h.mcpCall(c.op, c.body)
			if httpStatus != c.status {
				t.Fatalf("HTTP status %d, want %d: %v", httpStatus, c.status, httpBody)
			}
			if !mcpRes.isError() {
				t.Fatalf("MCP call did not report a tool error: %+v", mcpRes)
			}
			httpErr, _ := httpBody["error"].(map[string]any)
			if httpErr == nil {
				t.Fatalf("HTTP body carries no error envelope: %v", httpBody)
			}
			assertJSONEqual(t, "error envelope", withoutRequestID(mcpRes.toolError()), withoutRequestID(httpErr))
		})
	}

	// Same idempotency key, different records → conflict on both transports.
	// The status is pinned by the api package (TestErrorEnvelopeConflict):
	// request-class 400, stable code "conflict" — distinct from the 409 a
	// migrate expected_version conflict reports.
	replay := map[string]any{
		"namespace": "errp", "table": "t", "idempotency_key": "diverge",
		"records": []map[string]any{{"title": "different"}},
	}
	httpStatus, httpBody := h.httpCall("insert", replay)
	if httpStatus != 400 {
		t.Fatalf("divergent replay: HTTP status %d, want 400: %v", httpStatus, httpBody)
	}
	mcpRes := h.mcpCall("insert", replay)
	if !mcpRes.isError() {
		t.Fatalf("divergent replay over MCP did not fail: %+v", mcpRes)
	}
	httpErr, _ := httpBody["error"].(map[string]any)
	assertJSONEqual(t, "conflict envelope", withoutRequestID(mcpRes.toolError()), withoutRequestID(httpErr))
}

// withoutRequestID drops the per-call request id from an error envelope
// before a cross-transport comparison: parity pins the code and message, and
// each transport call that sent no X-Request-Id carries its own
// server-generated id.
func withoutRequestID(env map[string]any) map[string]any {
	out := make(map[string]any, len(env))
	for k, v := range env {
		if k != "request_id" {
			out[k] = v
		}
	}
	return out
}

// TestMCPToolResultShape pins the MCP result container itself: success carries
// structuredContent with an empty content array and isError false.
func TestMCPToolResultShape(t *testing.T) {
	h := newHarness(t)
	res := h.mcpCall("list_namespaces", map[string]any{})
	if res.status != http.StatusOK {
		t.Fatalf("status %d", res.status)
	}
	content, ok := res.result["content"].([]any)
	if !ok || len(content) != 0 {
		t.Fatalf("content must be an empty array (no text mirror), got %v", res.result["content"])
	}
	if res.result["isError"] != false {
		t.Fatalf("isError must be false, got %v", res.result["isError"])
	}
	if res.structured() == nil {
		t.Fatal("structuredContent must carry the result object")
	}
}
