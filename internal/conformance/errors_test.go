package conformance

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

var postgresErrorPins = map[string]string{
	"fts syntax error":             `bare single quotes are not a term`,
	"fts syntax error with filter": `bare single quotes are not a term`,
	"fts unknown column filter":    `does not support the field:term column filter`,
	"fts gate substring in query":  `query "SQLITE_-x": a bare - is not a query operator`,
	"fts misuse framing in query":  `query "misuse at line 1 -x": a bare - is not a query operator`,
	"sql unknown function":         `unknown SQL function "no_such_fn"`,
	"sql missing column":           `use describe_table for column names`,
	"write sql rejected":           `^query must begin with SELECT or WITH \(got "INSERT"\); query is read-only`,
	"multiple statements rejected": `multiple statements are not allowed`,
}

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
		{"now default on a typed string field", "create_table", map[string]any{
			"namespace": "errc", "table": "badnow1",
			"fields": []map[string]any{{"name": "a", "type": "string", "default": "now()"}},
		}, 400, "invalid_request", `now\(\)`},
		{"now default on an untyped field", "create_table", map[string]any{
			"namespace": "errc", "table": "badnow2",
			"fields": []map[string]any{{"name": "a", "default": "now()"}},
		}, 400, "invalid_request", `"now\(\)" is only allowed on timestamp fields`},
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
		{"required-field table setup", "create_table", map[string]any{
			"namespace": "errc", "table": "req",
			"fields": []map[string]any{{"name": "a", "type": "string", "required": true}},
		}, 200, "", ""},
		{"insert missing required field", "insert", map[string]any{
			"namespace": "errc", "table": "req",
			"records": []map[string]any{{}},
		}, 400, "invalid_request", `field "a" is required`},
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

		{"describe missing table", "describe_table", map[string]any{"namespace": "errc", "table": "absent"}, 404, "not_found", `errc\.absent`},
		{"insert into missing table", "insert", map[string]any{
			"namespace": "errc", "table": "absent",
			"records": []map[string]any{{"title": "x"}},
		}, 404, "not_found", `absent`},
		{"list_migrations missing table", "list_migrations", map[string]any{"namespace": "errc", "table": "absent"}, 404, "not_found", `absent`},
		{"query missing table", "query", map[string]any{"namespace": "errc", "sql": "SELECT * FROM absent"}, 404, "not_found", `absent`},

		{"write sql rejected", "query", map[string]any{"namespace": "errc", "sql": "INSERT INTO t (title) VALUES ('x')"}, 400, "invalid_request", `^query must begin with SELECT or WITH \(got "INSERT"\); query is read-only`},
		{"multiple statements rejected", "query", map[string]any{"namespace": "errc", "sql": "SELECT 1; SELECT 2"}, 400, "invalid_request", `multiple statements are not allowed`},
		{"fts syntax error", "search_fulltext", map[string]any{"namespace": "errc", "table": "t", "query": "don't"}, 400, "invalid_request", `fts5: syntax error`},
		{"fts unknown column filter", "search_fulltext", map[string]any{"namespace": "errc", "table": "t", "query": "nocol:x"}, 400, "invalid_request", `column "nocol" not found`},
		{"fts syntax error with filter", "search_fulltext", map[string]any{"namespace": "errc", "table": "t", "query": "don't", "filter": "id > 0"}, 400, "invalid_request", `fts5: syntax error`},
		{"fts gate substring in query", "search_fulltext", map[string]any{"namespace": "errc", "table": "t", "query": "SQLITE_-x"}, 400, "invalid_request", `query "SQLITE_-x": FTS5 parses a bare "-".*double-quoted`},
		{"fts misuse framing in query", "search_fulltext", map[string]any{"namespace": "errc", "table": "t", "query": "misuse at line 1 -x"}, 400, "invalid_request", `query "misuse at line 1 -x": FTS5 parses a bare "-".*double-quoted`},
		{"field name echoing gate substring", "create_table", map[string]any{
			"namespace": "errc", "table": "gate",
			"fields": []map[string]any{{"name": "SQLITE_X", "type": "string"}},
		}, 400, "invalid_request", `invalid field name "SQLITE_X": must start with a lowercase letter`},
		{"namespace echoing gate substring", "list_tables", map[string]any{"namespace": "misuse at line"}, 400, "invalid_request", `invalid namespace "misuse at line"`},

		{"sql unknown function", "query", map[string]any{"namespace": "errc", "sql": "SELECT no_such_fn(title) FROM t"}, 400, "query_error", `unknown SQL function "no_such_fn"`},
		{"sql missing column", "query", map[string]any{"namespace": "errc", "sql": "SELECT nope FROM t"}, 400, "query_error", `not found`},
		{"malformed update filter", "update", map[string]any{
			"namespace": "errc", "table": "t", "filter": "id =", "set": map[string]any{"title": "x"},
		}, 400, "query_error", `WHERE expression`},

		{"idempotency divergence", "insert", map[string]any{
			"namespace": "errc", "table": "t", "idempotency_key": "diverge-1",
			"records": []map[string]any{{"title": "one"}},
		}, 200, "", ""},
		{"idempotency divergence replay", "insert", map[string]any{
			"namespace": "errc", "table": "t", "idempotency_key": "diverge-1",
			"records": []map[string]any{{"title": "two"}},
		}, 400, "conflict", `idempotency key .* was already recorded for a different insert.*re-send the identical body`},
		{"idempotency gate substring setup", "insert", map[string]any{
			"namespace": "errc", "table": "t", "idempotency_key": "diverge-SQLITE_2",
			"records": []map[string]any{{"title": "one"}},
		}, 200, "", ""},
		{"idempotency gate substring replay", "insert", map[string]any{
			"namespace": "errc", "table": "t", "idempotency_key": "diverge-SQLITE_2",
			"records": []map[string]any{{"title": "two"}},
		}, 400, "conflict", `idempotency key "diverge-SQLITE_2" was already recorded for a different insert.*re-send the identical body`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := h.httpCall(c.op, c.body)
			if status != c.status {
				t.Fatalf("status %d, want %d: %v", status, c.status, body)
			}
			if c.code == "" {
				return
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
			want := c.msgRe
			if testEngine(t) == store.EnginePostgres {
				if pg, ok := postgresErrorPins[c.name]; ok {
					want = pg
				}
			}
			if want != "" {
				wantMessage(t, c.name, msg, want)
			}

			if strings.Contains(msg, "SQL logic error") || strings.Contains(msg, "/tmp/") || strings.Contains(msg, ".db") {
				t.Fatalf("message leaks internals: %q", msg)
			}
		})
	}
}

func TestGoldenErrorConflict409(t *testing.T) {
	h := newHarness(t)
	h.seedTable("errc9", "t", []map[string]any{{"name": "title", "type": "string"}})

	h.mustHTTP("migrate", map[string]any{
		"namespace": "errc9", "table": "t", "expected_version": 1,
		"changes": []map[string]any{{"op": "set_fulltext", "name": "title", "value": true}},
	})

	status, body := h.httpCall("migrate", map[string]any{
		"namespace": "errc9", "table": "t", "expected_version": 1,
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

	res := h.mcpCall("search_vector", map[string]any{
		"namespace": "errint", "table": "t", "text": "anything",
	})
	if !res.isError() {
		t.Fatalf("MCP provider outage must be a tool error: %+v", res)
	}
	assertJSONEqual(t, "internal error envelope", withoutRequestID(res.toolError()), withoutRequestID(errObj))
}

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
		wantMessage(t, "malformed json", errObj["message"].(string), `^invalid JSON`)
	})

	t.Run("empty body on an op with required fields 400", func(t *testing.T) {
		res, body := h.httpCallRaw("list_tables", ``, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("expected invalid_request envelope, got %v", errObj)
		}
		wantMessage(t, "empty body", errObj["message"].(string), `invalid namespace ""`)
	})

	t.Run("bodyless headerless post on an op with required fields 400", func(t *testing.T) {
		res, body := h.httpCallRaw("list_tables", ``, "")
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("expected invalid_request envelope, got %v", errObj)
		}
		wantMessage(t, "bodyless headerless", errObj["message"].(string), `invalid namespace ""`)
	})

	t.Run("trailing content 400", func(t *testing.T) {
		res, _ := h.httpCallRaw("list_tables", `{"namespace":"x"} {"namespace":"y"}`, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("trailing content must be 400, got %d", res.StatusCode)
		}
	})

	t.Run("request body at and over the 32 MiB limit, both transports", func(t *testing.T) {

		atLimit := strings.Repeat("a", 32<<20)
		res, _ := h.httpCallRaw("list_tables", atLimit, "application/json")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("/v1 at-limit body: status %d, want 400 (must not 413)", res.StatusCode)
		}
		res, body := h.httpCallRaw("list_tables", atLimit+"a", "application/json")
		if res.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("/v1 one-over body: status %d, want 413: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("/v1 413 code %v, want invalid_request", errObj["code"])
		}

		postMCP := func(payload string) int {
			req, err := http.NewRequest(http.MethodPost, h.mcpURL, strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			r2, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			r2.Body.Close()
			return r2.StatusCode
		}
		if code := postMCP(atLimit); code != http.StatusBadRequest {
			t.Fatalf("/mcp at-limit body: status %d, want 400 (must not 413)", code)
		}
		if code := postMCP(atLimit + "a"); code != http.StatusRequestEntityTooLarge {
			t.Fatalf("/mcp one-over body: status %d, want 413", code)
		}
	})
}

func TestEmptyBodyIsAnEmptyObject(t *testing.T) {
	h := newHarness(t)

	for _, op := range []string{"list_namespaces", "describe_server", "capabilities"} {
		t.Run(op+" with no body", func(t *testing.T) {
			res, body := h.httpCallRaw(op, ``, "application/json")
			if res.StatusCode != 200 {
				t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
			}
			var env map[string]any
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("response is not JSON: %q", body)
			}
			if env["ok"] != true {
				t.Fatalf("expected ok envelope, got %v", env)
			}
			if _, ok := env["data"].(map[string]any); !ok {
				t.Fatalf("expected data object in envelope, got %v", env)
			}
		})
	}

	t.Run("zero-arg op with no body and no content-type", func(t *testing.T) {
		res, body := h.httpCallRaw("list_namespaces", ``, "")
		if res.StatusCode != 200 {
			t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("response is not JSON: %q", body)
		}
		if env["ok"] != true {
			t.Fatalf("expected ok envelope, got %v", env)
		}
	})

	t.Run("no body and {} answer identically", func(t *testing.T) {
		res, raw := h.httpCallRaw("list_namespaces", ``, "application/json")
		if res.StatusCode != 200 {
			t.Fatalf("status %d, want 200: %s", res.StatusCode, raw)
		}
		var emptyEnv map[string]any
		if err := json.Unmarshal([]byte(raw), &emptyEnv); err != nil {
			t.Fatalf("response is not JSON: %q", raw)
		}
		status, objEnv := h.httpCall("list_namespaces", map[string]any{})
		if status != 200 {
			t.Fatalf("status %d, want 200", status)
		}
		if !reflect.DeepEqual(emptyEnv["data"], objEnv["data"]) {
			t.Fatalf("no-body data %v must equal {} data %v", emptyEnv["data"], objEnv["data"])
		}
	})
}

func TestDecodeErrorFraming(t *testing.T) {
	h := newHarness(t)

	t.Run("valid json unknown field leads with the field", func(t *testing.T) {
		res, body := h.httpCallRaw("query", `{"namespace":"decf","table":"x","sql":"SELECT 1"}`, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("expected invalid_request envelope, got %v", errObj)
		}
		msg := errObj["message"].(string)
		wantMessage(t, "unknown field", msg, `^unknown field "table" on operation query; see query's InputSchema`)
		if strings.Contains(msg, "invalid JSON") {
			t.Fatalf("valid JSON must not be called invalid JSON: %q", msg)
		}
	})

	t.Run("malformed json keeps the parse framing", func(t *testing.T) {
		res, body := h.httpCallRaw("query", `{"namespace":"decf","sql":`, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("expected invalid_request envelope, got %v", errObj)
		}
		msg := errObj["message"].(string)
		wantMessage(t, "malformed json", msg, `^invalid JSON`)
		if strings.Contains(msg, "unknown field") {
			t.Fatalf("unparseable bytes must not be framed as an unknown field: %q", msg)
		}
	})

	t.Run("trailing garbage after an unknown field keeps the parse framing", func(t *testing.T) {
		res, body := h.httpCallRaw("query", `{"namespace":"decf","table":"x","sql":"SELECT 1"} garbage`, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("expected invalid_request envelope, got %v", errObj)
		}
		msg := errObj["message"].(string)
		wantMessage(t, "trailing garbage", msg, `^unexpected trailing content`)
		if strings.Contains(msg, "unknown field") {
			t.Fatalf("a document with invalid trailing bytes must not be framed as an unknown field: %q", msg)
		}
	})

	t.Run("wrongly typed value names the field and both types", func(t *testing.T) {
		res, body := h.httpCallRaw("query", `{"namespace":"decf","sql":1}`, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		errObj := envelopeFromString(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("expected invalid_request envelope, got %v", errObj)
		}
		msg := errObj["message"].(string)
		wantMessage(t, "type mismatch", msg, `^field "sql" must be a string, but the request sent a number; see query's InputSchema`)
		if strings.Contains(msg, "invalid JSON") {
			t.Fatalf("valid JSON carrying a wrongly typed value must not be called invalid JSON: %q", msg)
		}
		if strings.Contains(msg, "unknown field") {
			t.Fatalf("a wrongly typed value must not be framed as an unknown field: %q", msg)
		}
	})

	t.Run("type mismatch leaks no go internals", func(t *testing.T) {
		for _, probe := range []struct{ op, body string }{
			{"query", `{"namespace":"decf","sql":1}`},
			{"insert", `{"namespace":"decf","table":"t","records":"nope"}`},
			{"read_rows", `{"namespace":"decf","table":"t","ids":"nope"}`},
			{"create_table", `{"namespace":"decf","table":"t","fields":[{"name":"a","type":true}]}`},
		} {
			_, body := h.httpCallRaw(probe.op, probe.body, "application/json")
			msg := envelopeFromString(t, body)["message"].(string)
			for _, leak := range []string{"Req.", "json:", "Go struct", "of type", "unmarshal"} {
				if strings.Contains(msg, leak) {
					t.Fatalf("%s message leaks the go internal %q: %q", probe.op, leak, msg)
				}
			}
		}
	})

	t.Run("nested type mismatch names the path", func(t *testing.T) {
		_, body := h.httpCallRaw("create_table", `{"namespace":"decf","table":"t","fields":[{"name":"a","type":true}]}`, "application/json")
		msg := envelopeFromString(t, body)["message"].(string)
		wantMessage(t, "nested type mismatch", msg, `^field "fields(\.0)?\.type" must be a string, but the request sent a boolean`)
	})

	t.Run("non object body is named as such", func(t *testing.T) {
		res, body := h.httpCallRaw("query", `[1,2]`, "application/json")
		if res.StatusCode != 400 {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		msg := envelopeFromString(t, body)["message"].(string)
		wantMessage(t, "non object body", msg, `^request body must be a JSON object, but the request sent an array`)
	})

	t.Run("search ops leak no decoder text for a non object body", func(t *testing.T) {
		for _, op := range []string{"search_fulltext", "search_vector"} {
			res, body := h.httpCallRaw(op, `[1,2]`, "application/json")
			if res.StatusCode != 400 {
				t.Fatalf("%s with a non-object body: status %d, want 400: %s", op, res.StatusCode, body)
			}
			msg := envelopeFromString(t, body)["message"].(string)
			wantMessage(t, op+" non object body", msg, `^request body must be a JSON object, but the request sent an array`)
			for _, leak := range []string{"cannot unmarshal", "Go value", "map[string]interface", "json:"} {
				if strings.Contains(msg, leak) {
					t.Fatalf("%s message leaks the go internal %q: %q", op, leak, msg)
				}
			}
		}
	})

	t.Run("mcp reports the type mismatch framing too", func(t *testing.T) {
		res := h.mcpCall("query", map[string]any{"namespace": "decf", "sql": 1})
		if !res.isError() {
			t.Fatalf("MCP query with a wrongly typed value must fail: %+v", res)
		}
		env := res.toolError()
		if env["code"] != "invalid_request" {
			t.Fatalf("MCP code %v, want invalid_request: %v", env["code"], env)
		}
		wantMessage(t, "mcp type mismatch", env["message"].(string), `^field "sql" must be a string, but the request sent a number; see query's InputSchema`)
	})

	t.Run("mcp tool call reports the same framing", func(t *testing.T) {
		res := h.mcpCall("query", map[string]any{"namespace": "decf", "table": "x", "sql": "SELECT 1"})
		if !res.isError() {
			t.Fatalf("MCP query with unknown field must fail: %+v", res)
		}
		env := res.toolError()
		if env["code"] != "invalid_request" {
			t.Fatalf("MCP code %v, want invalid_request: %v", env["code"], env)
		}
		wantMessage(t, "mcp unknown field", env["message"].(string), `^unknown field "table" on operation query; see query's InputSchema`)
	})
}

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

func manyMaps(n int, build func(i int) map[string]any) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = build(i)
	}
	return out
}

func TestQueryRejectionSeparatesTypoFromWrite(t *testing.T) {
	h := newHarness(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "qrej"})

	for _, tc := range []struct{ name, sql, want string }{
		{"typo", "SELEKT * FROM t", `^query must begin with SELECT or WITH \(got "SELEKT"\)`},
		{"write", "DELETE FROM t WHERE 1=1", `^query must begin with SELECT or WITH \(got "DELETE"\)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, out := h.httpCall("query", map[string]any{"namespace": "qrej", "sql": tc.sql})
			if status != 400 {
				t.Fatalf("status %d, want 400: %v", status, out)
			}
			msg := out["error"].(map[string]any)["message"].(string)
			wantMessage(t, tc.name, msg, tc.want)
			if !strings.Contains(msg, "typo") {
				t.Fatalf("the rejection must mention that a misspelled keyword lands here too, got %q", msg)
			}
		})
	}
}

func TestRequestDerivedDocumentsAreNotSharedCacheable(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/skills", "/skills/dolmen", "/skills/dolmen-admin", "/v1/openapi.json"} {
		res, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		res.Body.Close()
		if got := res.Header.Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("%s Cache-Control = %q, want \"private, no-store\" — this document embeds a request-derived public URL", path, got)
		}
		vary := res.Header.Get("Vary")
		for _, want := range []string{"Host", "X-Forwarded-Host", "X-Original-Uri"} {
			if !strings.Contains(vary, want) {
				t.Errorf("%s Vary = %q, missing %q", path, vary, want)
			}
		}
	}
}

func TestAMissingTableSaysWhereToLook(t *testing.T) {
	h := newHarness(t)
	h.seedTable("papertrack", "papers", []map[string]any{{"name": "title", "type": "string"}})
	cases := map[string]string{
		"paper":      "table papertrack.paper does not exist; list_tables shows the tables papertrack holds",
		"my-papers!": `invalid table name "my-papers!": must start with a lowercase letter, contain only a-z, 0-9, and underscores, and be at most 64 characters, so no such table exists in papertrack; list_tables shows the tables it holds`,
	}
	for table, want := range cases {
		status, body := h.httpCall("describe_table", map[string]any{"namespace": "papertrack", "table": table})
		errObj := envelopeOf(t, body)
		if status != http.StatusNotFound || errObj["code"] != "not_found" || errObj["message"] != want {
			t.Fatalf("%s over /v1: %d %v\nwant not_found %q", table, status, errObj, want)
		}
		env := h.mcpCall("describe_table", map[string]any{"namespace": "papertrack", "table": table}).toolError()
		if env == nil || env["code"] != "not_found" || env["message"] != want {
			t.Fatalf("%s over MCP: %v\nwant not_found %q", table, env, want)
		}
	}
}
