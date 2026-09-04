package conformance

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The golden error contract: every documented failure mode maps to its pinned
// stable code, HTTP status, and message shape. Codes are the six documented
// strings clients branch on; the status pins REST semantics; msgRe pins the
// stable human-readable fragments (never the full wording).
//
// Note the two conflict statuses, both current contract: an idempotency-key
// divergence is a request-class error (400 + code conflict), while a migrate
// expected_version conflict is a state-class 409.
func TestGoldenErrorContract(t *testing.T) {
	h := newHarness(t)
	h.seedTable("errc", "t", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "vec", "type": "vector", "dim": 4},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "errc", "table": "t",
		"records": []map[string]any{{"title": "seed", "body": "seed text", "vec": []any{1, 0, 0, 0}}},
	})

	cases := []struct {
		name   string
		op     string
		body   map[string]any
		status int
		code   string
		msgRe  string
	}{
		// --- invalid_request -------------------------------------------------
		{"invalid namespace", "list_tables", map[string]any{"namespace": ""}, 400, "invalid_request", `invalid namespace ""`},
		{"null option value", "list_tables", map[string]any{"namespace": nil}, 400, "invalid_request", `null is not allowed`},
		{"reserved field name", "create_table", map[string]any{
			"namespace": "errc", "table": "bad",
			"fields": []map[string]any{{"name": "id", "type": "string"}},
		}, 400, "invalid_request", `record_id|reserved`},
		{"reserved table name", "create_table", map[string]any{
			"namespace": "errc", "table": "id",
			"fields": []map[string]any{{"name": "a", "type": "string"}},
		}, 400, "invalid_request", ``},
		{"too many records", "insert", map[string]any{
			"namespace": "errc", "table": "t",
			"records": manyMaps(1001, func(i int) map[string]any { return map[string]any{"title": "x"} }),
		}, 400, "invalid_request", `too many records: 1001 > 1000`},
		{"idempotency key too long", "insert", map[string]any{
			"namespace": "errc", "table": "t", "idempotency_key": strings.Repeat("k", 257),
			"records": []map[string]any{{"title": "x"}},
		}, 400, "invalid_request", `idempotency key is 257 bytes \(max 256\)`},
		{"idempotency key not a string", "insert", map[string]any{
			"namespace": "errc", "table": "t", "idempotency_key": 42,
			"records": []map[string]any{{"title": "x"}},
		}, 400, "invalid_request", `idempotency_key must be a string`},
		{"missing required field", "create_table", map[string]any{
			"namespace": "errc", "table": "req",
			"fields": []map[string]any{{"name": "a", "type": "string", "required": true}},
		}, 200, "", ""}, // created; the insert below exercises the rejection
		{"query vector dim mismatch", "search_vector", map[string]any{
			"namespace": "errc", "table": "t", "column": "vec", "vector": []any{1, 0, 0},
		}, 400, "invalid_request", `query vector has 3 entries, column vec expects dim 4`},
		{"text and vector together", "search_vector", map[string]any{
			"namespace": "errc", "table": "t", "text": "x", "vector": []any{1, 0, 0, 0},
		}, 400, "invalid_request", `pass either text or vector, not both`},
		{"neither text nor vector", "search_vector", map[string]any{
			"namespace": "errc", "table": "t",
		}, 400, "invalid_request", `pass either text or vector`},
		{"text naming a vector column", "search_vector", map[string]any{
			"namespace": "errc", "table": "t", "text": "x", "column": "vec",
		}, 400, "invalid_request", `vec|vector column`},
		{"dry_run empty changes", "migrate", map[string]any{
			"namespace": "errc", "table": "t", "dry_run": true, "changes": []map[string]any{},
		}, 400, "invalid_request", `no changes`},
		{"expected_version zero", "migrate", map[string]any{
			"namespace": "errc", "table": "t", "expected_version": 0,
			"changes": []map[string]any{{"op": "set_fulltext", "name": "title", "value": false}},
		}, 400, "invalid_request", `expected_version must be >= 1`},
		{"drop_table confirm mismatch", "drop_table", map[string]any{
			"namespace": "errc", "table": "t", "confirm": "other",
		}, 400, "invalid_request", `confirm must repeat the exact table name`},
		{"drop_namespace confirm mismatch", "drop_namespace", map[string]any{
			"namespace": "errc", "confirm": "other",
		}, 400, "invalid_request", `confirm must repeat the exact namespace name`},

		// --- not_found ---------------------------------------------------------
		{"describe missing table", "describe_table", map[string]any{"namespace": "errc", "table": "absent"}, 404, "not_found", `errc\.absent`},
		{"insert into missing table", "insert", map[string]any{
			"namespace": "errc", "table": "absent",
			"records": []map[string]any{{"title": "x"}},
		}, 404, "not_found", `absent`},
		{"list_migrations missing table", "list_migrations", map[string]any{"namespace": "errc", "table": "absent"}, 404, "not_found", `absent`},
		{"query missing table", "query", map[string]any{"namespace": "errc", "sql": "SELECT * FROM absent"}, 404, "not_found", `absent`},

		// Statement classification happens before execution and is a
		// request-class error; execution failures are query_error.
		{"write sql rejected", "query", map[string]any{"namespace": "errc", "sql": "INSERT INTO t (title) VALUES ('x')"}, 400, "invalid_request", `only read-only SELECT/WITH statements are allowed`},
		{"multiple statements rejected", "query", map[string]any{"namespace": "errc", "sql": "SELECT 1; SELECT 2"}, 400, "invalid_request", `multiple statements are not allowed`},
		{"fts syntax error", "search_fulltext", map[string]any{"namespace": "errc", "table": "t", "query": "don't"}, 400, "invalid_request", `fts5: syntax error`},
		{"fts unknown column filter", "search_fulltext", map[string]any{"namespace": "errc", "table": "t", "query": "nocol:x"}, 400, "invalid_request", `column "nocol" not found`},

		// --- query_error (execution failures) ------------------------------------
		// `SELECT (` is caught earlier by the statement-shape guard
		// (invalid_request); these pass the shape check and fail at prepare.
		{"sql unknown function", "query", map[string]any{"namespace": "errc", "sql": "SELECT no_such_fn(title) FROM t"}, 400, "query_error", `unknown SQL function "no_such_fn"`},
		{"sql missing column", "query", map[string]any{"namespace": "errc", "sql": "SELECT nope FROM t"}, 400, "query_error", `not found`},
		{"malformed update filter", "update", map[string]any{
			"namespace": "errc", "table": "t", "filter": "id =", "set": map[string]any{"title": "x"},
		}, 400, "query_error", `WHERE expression`},

		// --- conflict (request-class 400) ------------------------------------------
		{"idempotency divergence", "insert", map[string]any{
			"namespace": "errc", "table": "t", "idempotency_key": "diverge-1",
			"records": []map[string]any{{"title": "one"}},
		}, 200, "", ""}, // first insert ok; the next case replays it divergently
		{"idempotency divergence replay", "insert", map[string]any{
			"namespace": "errc", "table": "t", "idempotency_key": "diverge-1",
			"records": []map[string]any{{"title": "two"}},
		}, 400, "conflict", `idempotency key .* was already recorded for a different insert`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := h.httpCall(c.op, c.body)
			if status != c.status {
				t.Fatalf("status %d, want %d: %v", status, c.status, body)
			}
			if c.code == "" {
				return // setup row, asserted ok above
			}
			errObj, _ := body["error"].(map[string]any)
			if errObj == nil {
				t.Fatalf("no error envelope: %v", body)
			}
			if errObj["code"] != c.code {
				t.Fatalf("code %v, want %q: %v", errObj["code"], c.code, errObj)
			}
			msg, _ := errObj["message"].(string)
			if msg == "" {
				t.Fatalf("message must be a non-empty string: %v", errObj)
			}
			if c.msgRe != "" {
				wantMessage(t, c.name, msg, c.msgRe)
			}
			// The envelope never leaks the cause or internal paths.
			if strings.Contains(msg, "SQL logic error") || strings.Contains(msg, "/tmp/") || strings.Contains(msg, ".db") {
				t.Fatalf("message leaks internals: %q", msg)
			}
		})
	}
}

// TestGoldenErrorConflict409 pins the state-class conflict: a migrate whose
// expected_version precondition fails reports 409 + conflict.
func TestGoldenErrorConflict409(t *testing.T) {
	h := newHarness(t)
	h.seedTable("errc9", "t", []map[string]any{{"name": "title", "type": "string"}})

	// Bump to version 2.
	h.mustHTTP("migrate", map[string]any{
		"namespace": "errc9", "table": "t", "expected_version": 1,
		"changes": []map[string]any{{"op": "set_fulltext", "name": "title", "value": true}},
	})

	status, body := h.httpCall("migrate", map[string]any{
		"namespace": "errc9", "table": "t", "expected_version": 1, // stale
		"changes": []map[string]any{{"op": "set_fulltext", "name": "title", "value": false}},
	})
	if status != 409 {
		t.Fatalf("stale expected_version: status %d, want 409: %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "conflict" {
		t.Fatalf("code %v, want conflict: %v", errObj["code"], errObj)
	}
	wantMessage(t, "version conflict", errObj["message"].(string),
		`version conflict on errc9\.t: schema is at version 2, expected 1`)

	// The MCP transport reports the same class as a tool error.
	res := h.mcpCall("migrate", map[string]any{
		"namespace": "errc9", "table": "t", "expected_version": 1,
		"changes": []map[string]any{{"op": "set_fulltext", "name": "title", "value": false}},
	})
	if !res.isError() {
		t.Fatalf("MCP migrate with stale version must fail: %+v", res)
	}
	env := res.toolError()
	if env["code"] != "conflict" {
		t.Fatalf("MCP code %v, want conflict: %v", env["code"], env)
	}
}

// TestGoldenErrorForbidden pins the transport-level forbidden envelope: a
// cross-origin request from a non-allowlisted origin is rejected before any
// operation runs.
func TestGoldenErrorForbidden(t *testing.T) {
	h := newHarness(t)
	res := h.postWithOrigin("http://evil.example", "list_tables", `{"namespace":"x"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status %d, want 403", res.StatusCode)
	}
	var body map[string]any
	decodeJSON(t, res, &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "forbidden" {
		t.Fatalf("expected forbidden envelope, got %v", body)
	}
}

// TestGoldenErrorInternal pins the internal_error class: a provider outage
// under search_vector surfaces as 500 + internal_error with the fixed
// sanitized message — never the provider's error text or a URL.
func TestGoldenErrorInternal(t *testing.T) {
	dir := t.TempDir()
	emb := &fakeProvider{}
	h := newHarnessAt(t, dir, emb)
	h.seedTable("errint", "t", []map[string]any{{"name": "body", "type": "text", "vectorize": true}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "errint", "table": "t",
		"records": []map[string]any{{"body": "seed text"}},
	})

	emb.mu.Lock()
	emb.fail = errors.New("provider exploded: connection refused to https://secret.internal/v1 (api key sk-LEAKED)")
	emb.mu.Unlock()

	status, body := h.httpCall("search_vector", map[string]any{
		"namespace": "errint", "table": "t", "text": "anything",
	})
	if status != 500 {
		t.Fatalf("provider outage: status %d, want 500: %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "internal_error" {
		t.Fatalf("code %v, want internal_error: %v", errObj["code"], errObj)
	}
	if msg, _ := errObj["message"].(string); msg != "internal error" {
		t.Fatalf("message must be the fixed sanitized string, got %q", msg)
	}

	// MCP reports the identical envelope as a tool error.
	res := h.mcpCall("search_vector", map[string]any{
		"namespace": "errint", "table": "t", "text": "anything",
	})
	if !res.isError() {
		t.Fatalf("MCP provider outage must be a tool error: %+v", res)
	}
	assertJSONEqual(t, "internal error envelope", res.toolError(), errObj)
}

// TestTransportLevelErrors pins the HTTP transport guards: unknown operation,
// wrong method, wrong content type, oversized body, malformed JSON, trailing
// content. These precede any operation dispatch.
func TestTransportLevelErrors(t *testing.T) {
	h := newHarness(t)

	t.Run("unknown operation 404", func(t *testing.T) {
		res, body := h.httpCallRaw("no_such_op", `{"x":1}`, "application/json")
		if res.StatusCode != 404 {
			t.Fatalf("status %d, want 404: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "not_found" {
			t.Fatalf("expected not_found envelope, got %v", errObj)
		}
	})

	t.Run("get not post 405", func(t *testing.T) {
		res, err := http.Get(h.httpURL + "/list_tables")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 405 {
			t.Fatalf("status %d, want 405", res.StatusCode)
		}
		if res.Header.Get("Allow") != "POST" {
			t.Fatalf(`Allow header must be "POST", got %q`, res.Header.Get("Allow"))
		}
	})

	t.Run("wrong content type 415", func(t *testing.T) {
		res, _ := h.httpCallRaw("list_tables", `{"namespace":"x"}`, "text/plain")
		if res.StatusCode != 415 {
			t.Fatalf("status %d, want 415", res.StatusCode)
		}
	})

	t.Run("malformed json 400", func(t *testing.T) {
		res, body := h.httpCallRaw("list_tables", `{"namespace":`, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("expected invalid_request envelope, got %v", errObj)
		}
	})

	t.Run("empty body 400", func(t *testing.T) {
		res, body := h.httpCallRaw("list_tables", ``, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("expected invalid_request envelope, got %v", errObj)
		}
		wantMessage(t, "empty body", errObj["message"].(string), `empty request body`)
	})

	t.Run("trailing content 400", func(t *testing.T) {
		res, _ := h.httpCallRaw("list_tables", `{"namespace":"x"} {"namespace":"y"}`, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("trailing content must be 400, got %d", res.StatusCode)
		}
	})

	t.Run("oversized body 413", func(t *testing.T) {
		big := strings.Repeat("a", 33<<20)
		res, _ := h.httpCallRaw("list_tables", big, "application/json")
		if res.StatusCode != 413 {
			t.Fatalf("status %d, want 413", res.StatusCode)
		}
	})
}

// TestMCPProtocolErrors pins the JSON-RPC protocol layer above tools/call.
// These are transport errors with JSON-RPC codes, distinct from tool errors.
func TestMCPProtocolErrors(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name     string
		msg      any
		wantCode int
		status   int
	}{
		{"unknown method", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"}, -32601, 200},
		{"unknown tool", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "nope", "arguments": map[string]any{}}}, -32602, 200},
		{"missing tool name", map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"arguments": map[string]any{}}}, -32602, 200},
		{"arguments not an object", map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "list_tables", "arguments": []any{1}}}, -32602, 200},
		{"not a jsonrpc request", map[string]any{"id": 5, "method": "ping"}, -32600, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := h.rpc(c.msg)
			if res.status != c.status {
				t.Fatalf("HTTP status %d, want %d", res.status, c.status)
			}
			if res.proto == nil {
				t.Fatalf("expected a JSON-RPC error object, got %+v", res)
			}
			if code := res.proto["code"].(float64); int(code) != c.wantCode {
				t.Fatalf("JSON-RPC code %v, want %d", res.proto["code"], c.wantCode)
			}
			if res.proto["message"] == "" {
				t.Fatal("JSON-RPC error must carry a message")
			}
		})
	}

	t.Run("malformed json -32700", func(t *testing.T) {
		res := h.postMCPRaw(`{"jsonrpc":`)
		defer res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400", res.StatusCode)
		}
		var env map[string]any
		decodeJSON(t, res, &env)
		errObj, _ := env["error"].(map[string]any)
		if errObj == nil || int(errObj["code"].(float64)) != -32700 {
			t.Fatalf("expected -32700 parse error, got %v", env)
		}
	})

	t.Run("notification gets 202", func(t *testing.T) {
		res := h.rpc(map[string]any{"jsonrpc": "2.0", "method": "ping"})
		if res.status != 202 {
			t.Fatalf("notification status %d, want 202", res.status)
		}
	})
}

// envelopeFromString extracts the error object from a raw response body.
func envelopeFromString(t *testing.T, body string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("response is not JSON: %q", body)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("response carries no error envelope: %s", body)
	}
	return errObj
}

// manyMaps builds n records for limit cases without flooding test output.
func manyMaps(n int, build func(i int) map[string]any) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = build(i)
	}
	return out
}
