package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/version"
)

type fakeEmb struct{}

func (fakeEmb) Name() string { return "fake" }

func (fakeEmb) Identity() string { return "fake-space" }

func (fakeEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		for _, r := range []byte(t) {
			v[r%8] += 1
		}
		out[i] = v
	}
	return out, nil
}

func newMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(api.New(st, fakeEmb{}), nil))
	t.Cleanup(srv.Close)
	return srv
}

func rpc(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusAccepted {
		return res.StatusCode, nil
	}
	var decoded map[string]any
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return res.StatusCode, decoded
}

func TestMCPUpdateUpsertTools(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"

	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "create_table", "arguments": map[string]any{
			"namespace": "agents",
			"table":     "tasks",
			"fields":    []map[string]any{{"name": "title", "type": "string", "fulltext": true}, {"name": "done", "type": "boolean"}},
		}},
	})
	if code != 200 || res["result"].(map[string]any)["isError"] == true {
		t.Fatalf("create_table failed: %d %v", code, res)
	}

	code, res = rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "upsert", "arguments": map[string]any{
			"namespace": "agents", "table": "tasks",
			"filter": "title = 'first'", "set": map[string]any{"title": "first", "done": false},
		}},
	})
	if code != 200 || res["result"].(map[string]any)["isError"] == true {
		t.Fatalf("upsert tool call failed: %d %v", code, res)
	}
	if up := res["result"].(map[string]any)["structuredContent"].(map[string]any); up["inserted"] != true {
		t.Fatalf("upsert must report an insert, got %v", up)
	}

	code, res = rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "update", "arguments": map[string]any{
			"namespace": "agents", "table": "tasks",
			"filter": "title = ?", "args": []any{"first"}, "set": map[string]any{"done": true},
		}},
	})
	if code != 200 || res["result"].(map[string]any)["isError"] == true {
		t.Fatalf("update tool call failed: %d %v", code, res)
	}
	if upd := res["result"].(map[string]any)["structuredContent"].(map[string]any); upd["updated"].(float64) != 1 {
		t.Fatalf("update must report one row, got %v", upd)
	}
}

func TestMCPProtocol(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"

	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
		},
	})
	if code != 200 {
		t.Fatalf("initialize status %d", code)
	}
	result := res["result"].(map[string]any)
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "dolmen" {
		t.Fatalf("unexpected serverInfo: %v", info)
	}
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol version not echoed: %v", result)
	}

	code, res = rpc(t, url, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if code != 200 {
		t.Fatalf("tools/list status %d", code)
	}
	tools := res["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 14 {
		t.Fatalf("expected 14 tools, got %d", len(tools))
	}
	first := tools[0].(map[string]any)
	if first["name"] == "" || first["inputSchema"] == nil {
		t.Fatalf("tool missing name or schema: %v", first)
	}

	rpc(t, url, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	code, res = rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "create_table", "arguments": map[string]any{
			"namespace": "agents",
			"table":     "memory",
			"fields":    []map[string]any{{"name": "fact", "type": "string", "fulltext": true}},
		}},
	})
	if code != 200 {
		t.Fatalf("tools/call create status %d", code)
	}
	callRes := res["result"].(map[string]any)
	if callRes["isError"] == true {
		t.Fatalf("create_table call failed: %v", callRes)
	}

	code, res = rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "list_tables", "arguments": map[string]any{"namespace": "agents"}},
	})
	if code != 200 {
		t.Fatalf("tools/call list status %d", code)
	}
	tables, ok := res["result"].(map[string]any)["structuredContent"].(map[string]any)["tables"].([]any)
	if !ok || len(tables) != 1 || tables[0] != "memory" {
		t.Fatalf("expected memory table in structuredContent, got: %v", res["result"])
	}

	res2, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET, got %d", res2.StatusCode)
	}
}

func TestInitializeUnsupportedVersionFallsBack(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "1999-01-01",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
		},
	})
	if code != 200 {
		t.Fatalf("initialize status %d", code)
	}
	if got := res["result"].(map[string]any)["protocolVersion"]; got != "2025-06-18" {
		t.Fatalf("unsupported requested version must fall back to server version, got %v", got)
	}
}

func TestUnsupportedProtocolVersionHeaderRejected(t *testing.T) {
	srv := newMCPServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "1999-01-01")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported protocol version must 400, got %d", res.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("supported protocol version must pass, got %d", res.StatusCode)
	}
}

func TestMalformedCallParamsUseInvalidParamsCode(t *testing.T) {
	srv := newMCPServer(t)
	code, res := rpc(t, srv.URL, map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params":  []any{"not", "an", "object"},
	})
	if code != http.StatusOK {
		t.Fatalf("unexpected http status: %d", code)
	}
	errObj, ok := res["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error object, got %v", res)
	}
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("malformed params must use -32602 (Invalid params), not the parse-error code, got %v", errObj["code"])
	}
}

func TestMalformedTransportInputHTTP400(t *testing.T) {
	srv := newMCPServer(t)
	res, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid JSON must be HTTP 400, got %d", res.StatusCode)
	}
	for _, body := range []string{`[]`, `42`} {
		res, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", body, err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("valid non-object JSON %s must be HTTP 400, got %d", body, res.StatusCode)
		}
		errObj, ok := decoded["error"].(map[string]any)
		if !ok || errObj["code"].(float64) != -32600 {
			t.Fatalf("non-object JSON %s must be -32600 (Invalid Request), got %v", body, decoded)
		}
	}
}

func TestMalformedInitializeParamsRejected(t *testing.T) {
	srv := newMCPServer(t)
	code, res := rpc(t, srv.URL, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "initialize",
		"params":  []any{"not", "an", "object"},
	})
	if code != http.StatusOK {
		t.Fatalf("unexpected http status: %d", code)
	}
	errObj, ok := res["error"].(map[string]any)
	if !ok || errObj["code"].(float64) != -32602 {
		t.Fatalf("malformed initialize params must be -32602, got %v", res)
	}
}

func TestInitializeRequiresProtocolVersion(t *testing.T) {
	srv := newMCPServer(t)
	code, res := rpc(t, srv.URL, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	if code != http.StatusOK {
		t.Fatalf("unexpected http status: %d", code)
	}
	errObj, ok := res["error"].(map[string]any)
	if !ok || errObj["code"].(float64) != -32602 {
		t.Fatalf("initialize without protocolVersion must be -32602, got %v", res)
	}
}

func TestMCPOversizedBodyReturns413(t *testing.T) {
	srv := newMCPServer(t)
	big := bytes.Repeat([]byte("a"), 33<<20)
	res, err := http.Post(srv.URL, "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized MCP body must 413, got %d", res.StatusCode)
	}
}

func TestInitializeRequiresClientInfoAndCapabilities(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 11, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}},
	})
	if code != 200 {
		t.Fatalf("unexpected status: %d", code)
	}
	errObj, ok := res["error"].(map[string]any)
	if !ok || errObj["code"].(float64) != -32602 {
		t.Fatalf("initialize without clientInfo must be -32602, got %v", res)
	}
}

func TestToolsCallRequiresName(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 12, "method": "tools/call",
		"params": map[string]any{"arguments": map[string]any{"namespace": "x"}},
	})
	if code != 200 {
		t.Fatalf("unexpected status: %d", code)
	}
	errObj, ok := res["error"].(map[string]any)
	if !ok || errObj["code"].(float64) != -32602 {
		t.Fatalf("tools/call without a name must be -32602 (malformed request, not a tool error), got %v", res)
	}
}

func TestToolsCallNonObjectArgumentsRejected(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 13, "method": "tools/call",
		"params": map[string]any{"name": "list_tables", "arguments": []any{"not", "an", "object"}},
	})
	if code != 200 {
		t.Fatalf("unexpected status: %d", code)
	}
	errObj, ok := res["error"].(map[string]any)
	if !ok || errObj["code"].(float64) != -32602 {
		t.Fatalf("non-object arguments must be -32602, got %v", res)
	}
	code, res = rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 14, "method": "tools/call",
		"params": map[string]any{"name": "list_tables", "arguments": nil},
	})
	errObj, ok = res["error"].(map[string]any)
	if code != 200 || !ok || errObj["code"].(float64) != -32602 {
		t.Fatalf("explicit null arguments must be -32602, got %d %v", code, res)
	}
}

func TestToolsListParamsValidated(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 15, "method": "tools/list",
		"params": []any{"not", "an", "object"},
	})
	errObj, ok := res["error"].(map[string]any)
	if code != 200 || !ok || errObj["code"].(float64) != -32602 {
		t.Fatalf("array tools/list params must be -32602, got %d %v", code, res)
	}
	code, res = rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 16, "method": "tools/list",
		"params": map[string]any{"cursor": 1},
	})
	errObj, ok = res["error"].(map[string]any)
	if code != 200 || !ok || errObj["code"].(float64) != -32602 {
		t.Fatalf("non-string cursor must be -32602, got %d %v", code, res)
	}
	code, res = rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 17, "method": "tools/list",
		"params": map[string]any{"cursor": "page-2"},
	})
	if code != 200 || res["result"] == nil {
		t.Fatalf("string cursor must pass (registry is unpaginated), got %d %v", code, res)
	}
}

func TestInvalidRequestIDRejected(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	for _, id := range []any{true, map[string]any{}, []any{}, nil} {
		body := map[string]any{"jsonrpc": "2.0", "id": id, "method": "ping"}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		res, err := http.Post(url, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		res.Body.Close()
		errObj, ok := decoded["error"].(map[string]any)
		if !ok || errObj["code"].(float64) != -32600 {
			t.Fatalf("id %v must be rejected with -32600 before dispatch, got %v", id, decoded)
		}
		if _, hasID := decoded["id"]; !hasID || decoded["id"] != nil {
			t.Fatalf("error response for invalid id %v must carry a null id, got %v", id, decoded["id"])
		}
	}
}

func TestEnvelopeErrorWithInvalidIDUsesNullID(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	res, err := http.Post(url, "application/json", strings.NewReader(`{"jsonrpc":"1.0","id":true,"method":"ping"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	res.Body.Close()
	if _, hasID := decoded["id"]; !hasID || decoded["id"] != nil {
		t.Fatalf("envelope error for invalid id must carry a null id, got %v", decoded["id"])
	}
}

func TestNullParamsRejected(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	for _, method := range []string{"ping", "tools/list"} {
		code, res := rpc(t, url, map[string]any{
			"jsonrpc": "2.0", "id": 21, "method": method, "params": nil,
		})
		errObj, ok := res["error"].(map[string]any)
		if code != 200 || !ok || errObj["code"].(float64) != -32602 {
			t.Fatalf("%s with explicit null params must be -32602, got %d %v", method, code, res)
		}
	}
}

func TestPingParamsValidated(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	for _, params := range []any{[]any{"x"}, 1} {
		code, res := rpc(t, url, map[string]any{
			"jsonrpc": "2.0", "id": 18, "method": "ping", "params": params,
		})
		errObj, ok := res["error"].(map[string]any)
		if code != 200 || !ok || errObj["code"].(float64) != -32602 {
			t.Fatalf("ping params %v must be -32602, got %d %v", params, code, res)
		}
	}
	code, res := rpc(t, url, map[string]any{"jsonrpc": "2.0", "id": 19, "method": "ping"})
	if code != 200 || res["result"] == nil {
		t.Fatalf("bare ping must succeed, got %d %v", code, res)
	}
}

func TestHugeNumericIDPreserved(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	res, err := http.Post(url, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1e1000,"method":"ping"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	res.Body.Close()
	if decoded["error"] != nil {
		t.Fatalf("1e1000 is a valid numeric id, got error %v", decoded["error"])
	}
	if _, ok := decoded["result"]; !ok {
		t.Fatalf("expected a result, got %v", decoded)
	}
}

func TestLargeNumbersInArgumentsAccepted(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	res, err := http.Post(url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"infer_schema","arguments":{"samples":[{"score":1e1000}]}}}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	res.Body.Close()
	errObj, _ := decoded["error"].(map[string]any)
	if errObj != nil && strings.Contains(errObj["message"].(string), "arguments must be an object") {
		t.Fatalf("large numbers in arguments must not fail the object probe, got %v", errObj)
	}
	if decoded["result"] == nil {
		t.Fatalf("expected a tool result, got %v", decoded)
	}
}

func TestUnknownToolIsProtocolError(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 31, "method": "tools/call",
		"params": map[string]any{"name": "explode"},
	})
	errObj, ok := res["error"].(map[string]any)
	if code != 200 || !ok || errObj["code"].(float64) != -32602 {
		t.Fatalf("unknown tool must be a -32602 protocol error, not a tool-reported failure, got %d %v", code, res)
	}
}

func TestTrailingGarbageIsParseError(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	res, err := http.Post(url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"} junk`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("trailing garbage must be HTTP 400, got %d", res.StatusCode)
	}
	errObj, ok := decoded["error"].(map[string]any)
	if !ok || errObj["code"].(float64) != -32700 {
		t.Fatalf("trailing garbage must be -32700 (invalid JSON), got %v", decoded)
	}
}

func TestInitializeLargeCapabilityNumbersAccepted(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	res, err := http.Post(url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":40,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"experimental":{"vendor":{"limit":1e1000}}},"clientInfo":{"name":"c","version":"1"}}}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	res.Body.Close()
	if decoded["error"] != nil {
		t.Fatalf("large capability numbers are valid JSON, got error %v", decoded["error"])
	}
	if decoded["result"] == nil {
		t.Fatalf("expected an initialize result, got %v", decoded)
	}
}

func TestCapabilityShapesValidated(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	for _, caps := range []string{
		`{"roots":{"listChanged":"yes"}}`,
		`{"sampling":1}`,
		`{"elicitation":"no"}`,
		`{"experimental":"x"}`,
		`{"roots":true}`,
	} {
		body := `{"jsonrpc":"2.0","id":50,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":` + caps + `,"clientInfo":{"name":"c","version":"1"}}}`
		res, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", caps, err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		res.Body.Close()
		errObj, ok := decoded["error"].(map[string]any)
		if !ok || errObj["code"].(float64) != -32602 {
			t.Fatalf("malformed capabilities %s must be -32602, got %v", caps, decoded)
		}
	}
	for _, caps := range []string{
		`{"roots":{"listChanged":true}}`,
		`{"sampling":{}}`,
		`{"elicitation":{}}`,
		`{"experimental":{"vendor":{"limit":1e1000}}}`,
		`{"customFuture":{"anything":[1,2]}}`,
		`{"logging":[]}`,
	} {
		body := `{"jsonrpc":"2.0","id":51,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":` + caps + `,"clientInfo":{"name":"c","version":"1"}}}`
		res, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", caps, err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		res.Body.Close()
		if decoded["error"] != nil {
			t.Fatalf("well-formed capabilities %s must pass, got %v", caps, decoded["error"])
		}
	}
}

func TestExperimentalCapabilityValuesValidated(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	for _, caps := range []string{`{"experimental":{"vendor":1}}`, `{"experimental":{"vendor":null}}`} {
		body := `{"jsonrpc":"2.0","id":60,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":` + caps + `,"clientInfo":{"name":"c","version":"1"}}}`
		res, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", caps, err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		res.Body.Close()
		errObj, ok := decoded["error"].(map[string]any)
		if !ok || errObj["code"].(float64) != -32602 {
			t.Fatalf("scalar experimental capability %s must be -32602, got %v", caps, decoded)
		}
	}
}

func TestMetaParamsValidated(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	for _, params := range []string{`{"_meta":1}`, `{"_meta":{"progressToken":true}}`} {
		body := `{"jsonrpc":"2.0","id":61,"method":"ping","params":` + params + `}`
		res, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", params, err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		res.Body.Close()
		errObj, ok := decoded["error"].(map[string]any)
		if !ok || errObj["code"].(float64) != -32602 {
			t.Fatalf("malformed _meta %s must be -32602, got %v", params, decoded)
		}
	}
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 62, "method": "ping",
		"params": map[string]any{"_meta": map[string]any{"progressToken": "tok-1"}},
	})
	if code != 200 || res["result"] == nil {
		t.Fatalf("object _meta with string progressToken must pass, got %d %v", code, res)
	}
}

func TestToolsCallMetaValidated(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	for _, params := range []string{`{"name":"ping","_meta":1}`, `{"name":"list_tables","_meta":{"progressToken":true}}`} {
		body := `{"jsonrpc":"2.0","id":70,"method":"tools/call","params":` + params + `}`
		res, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", params, err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		res.Body.Close()
		errObj, ok := decoded["error"].(map[string]any)
		if !ok || errObj["code"].(float64) != -32602 {
			t.Fatalf("malformed tools/call _meta %s must be -32602 before dispatch, got %v", params, decoded)
		}
	}
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 71, "method": "tools/call",
		"params": map[string]any{
			"name":  "list_tables",
			"_meta": map[string]any{"progressToken": 5},
		},
		"arguments": map[string]any{"namespace": "x"},
	})
	if code != 200 || res["result"] == nil {
		t.Fatalf("object _meta with numeric progressToken must pass, got %d %v", code, res)
	}
}

func TestInitializeMetaValidated(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	for _, meta := range []string{`1`, `{"progressToken":true}`} {
		body := `{"jsonrpc":"2.0","id":80,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"},"_meta":` + meta + `}}`
		res, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", meta, err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		res.Body.Close()
		errObj, ok := decoded["error"].(map[string]any)
		if !ok || errObj["code"].(float64) != -32602 {
			t.Fatalf("malformed initialize _meta %s must be -32602, got %v", meta, decoded)
		}
	}
}

func TestClientInfoTitleValidated(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	for _, title := range []string{`1`, `null`, `true`} {
		body := `{"jsonrpc":"2.0","id":90,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1","title":` + title + `}}}`
		res, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", title, err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(res.Body).Decode(&decoded)
		res.Body.Close()
		errObj, ok := decoded["error"].(map[string]any)
		if !ok || errObj["code"].(float64) != -32602 {
			t.Fatalf("non-string clientInfo title %s must be -32602, got %v", title, decoded)
		}
	}
	body := `{"jsonrpc":"2.0","id":91,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1","title":"Console"}}}`
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	res.Body.Close()
	if decoded["error"] != nil {
		t.Fatalf("string title must pass, got %v", decoded["error"])
	}
}

func TestMCPOriginAndContentTypeGuard(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	post := func(origin, ct string) int {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post("http://evil.example", "application/json"); code != http.StatusForbidden {
		t.Fatalf("hostile origin must be 403, got %d", code)
	}
	if code := post("", "text/plain"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON content type must be 415 (no preflight-free CSRF), got %d", code)
	}
	if code := post("http://localhost:5173", "application/json"); code != http.StatusOK {
		t.Fatalf("localhost origin with JSON must pass, got %d", code)
	}
	if code := post("", "application/json"); code != http.StatusOK {
		t.Fatalf("no-origin server-to-server call must pass, got %d", code)
	}
}

func TestMCPToolErrorReturnsStableEnvelope(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"

	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "query",
			"arguments": map[string]any{"namespace": "x", "sql": "SELECT * FROOOM notes"},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "mcp-req-1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()

	if res.Header.Get("X-Request-Id") != "mcp-req-1" {
		t.Fatalf("expected X-Request-Id header echoed, got %q", res.Header.Get("X-Request-Id"))
	}

	var decoded map[string]any
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["error"] != nil {
		t.Fatalf("expected a tool result, got JSON-RPC error: %v", decoded["error"])
	}
	result, ok := decoded["result"].(map[string]any)
	if !ok || result["isError"] != true {
		t.Fatalf("expected isError tool result, got %v", decoded)
	}
	content := result["content"].([]any)[0].(map[string]any)
	text := content["text"].(string)

	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("tool error content is not JSON: %v\ntext: %s", err, text)
	}
	if env["code"] != "query_error" {
		t.Fatalf("expected query_error code, got %v", env["code"])
	}
	if env["request_id"] != "mcp-req-1" {
		t.Fatalf("expected request_id echoed, got %v", env["request_id"])
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "FROOOM") || !strings.Contains(msg, "SELECT or WITH") {
		t.Fatalf("expected self-correctable query error with offending token, got %q", msg)
	}
	if strings.Contains(msg, "SQL logic error") {
		t.Fatalf("raw SQLite leaked into MCP tool error: %q", msg)
	}
}

func TestMCPConfiguredOriginsAllowed(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(api.New(st, fakeEmb{}), []string{"https://app.example.com"}))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://app.example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("configured origin must pass the inner guard, got %d", res.StatusCode)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("allowed origin must be echoed, got %q", res.Header.Get("Access-Control-Allow-Origin"))
	}
	if res.Header.Get("Access-Control-Expose-Headers") != "X-Request-Id" {
		t.Fatalf("X-Request-Id must be exposed, got %q", res.Header.Get("Access-Control-Expose-Headers"))
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("unlisted origin must still be 403, got %d", res.StatusCode)
	}
}

func TestToolAnnotationsMatchRegistry(t *testing.T) {
	for _, name := range api.OpNames() {
		ann, ok := toolAnnotations[name]
		if !ok {
			t.Fatalf("tool %q has no MCP annotations", name)
		}
		if title, ok := ann["title"].(string); !ok || title == "" {
			t.Fatalf("tool %q annotations must carry a non-empty title, got %v", name, ann)
		}
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, isBool := ann[hint].(bool); !isBool {
				t.Fatalf("tool %q annotation %s must be a boolean, got %v", name, hint, ann[hint])
			}
		}
	}
	for name := range toolAnnotations {
		if _, ok := api.Ops[name]; !ok {
			t.Fatalf("annotations defined for unknown tool %q", name)
		}
	}
}

func TestToolsListAdvertisesOutputSchemasAndAnnotations(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, res := rpc(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if code != 200 {
		t.Fatalf("tools/list status %d", code)
	}
	tools, _ := res["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(api.OpNames()) {
		t.Fatalf("expected %d tools, got %d", len(api.OpNames()), len(tools))
	}
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		name, _ := tool["name"].(string)
		schema, ok := tool["outputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("tool %q must advertise an outputSchema, got %v", name, tool)
		}
		if schema["type"] != "object" {
			t.Fatalf("tool %q outputSchema must be an object schema, got %v", name, schema["type"])
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			t.Fatalf("tool %q outputSchema must declare properties, got %v", name, schema["properties"])
		}
		required, _ := schema["required"].([]any)
		if len(required) == 0 {
			t.Fatalf("tool %q outputSchema must require at least one property, got %v", name, schema["required"])
		}
		for _, r := range required {
			if _, ok := props[r.(string)]; !ok {
				t.Fatalf("tool %q outputSchema requires unknown property %q", name, r)
			}
		}
		ann, ok := tool["annotations"].(map[string]any)
		if !ok {
			t.Fatalf("tool %q must advertise annotations, got %v", name, tool)
		}
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, isBool := ann[hint].(bool); !isBool {
				t.Fatalf("tool %q annotation %s must be a boolean, got %v", name, hint, ann[hint])
			}
		}
	}
}

func TestToolHintSemantics(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, res := rpc(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if code != 200 {
		t.Fatalf("tools/list status %d", code)
	}
	tools, _ := res["result"].(map[string]any)["tools"].([]any)
	byName := map[string]map[string]any{}
	for _, entry := range tools {
		tool := entry.(map[string]any)
		byName[tool["name"].(string)] = tool
	}
	for _, name := range []string{"list_tables", "describe_table", "query", "search_fulltext", "search_vector"} {
		if ann := byName[name]["annotations"].(map[string]any); ann["readOnlyHint"] != false {
			t.Fatalf("store-reading tool %q creates the namespace db on first touch and must not claim readOnly, got %v", name, ann)
		}
	}
	if ann := byName["infer_schema"]["annotations"].(map[string]any); ann["readOnlyHint"] != true {
		t.Fatalf("infer_schema is pure (never touches the store) and must carry readOnlyHint true, got %v", ann)
	}
	for _, name := range []string{"delete", "migrate", "update", "upsert", "upsert_by_key"} {
		if ann := byName[name]["annotations"].(map[string]any); ann["destructiveHint"] != true {
			t.Fatalf("write tool %q can overwrite or drop existing data and must carry destructiveHint true, got %v", name, ann)
		}
	}
	for _, name := range []string{"update", "upsert", "upsert_by_key"} {
		ann := byName[name]["annotations"].(map[string]any)
		if ann["idempotentHint"] != false {
			t.Fatalf("write tool %q re-embeds on every retry (and filter-driven forms can walk new rows), so it must not claim idempotent, got %v", name, ann)
		}
	}
	for _, name := range []string{"insert", "search_vector", "migrate"} {
		if ann := byName[name]["annotations"].(map[string]any); ann["openWorldHint"] != true {
			t.Fatalf("provider-capable tool %q can send text to the configured remote embedding endpoint and must carry openWorldHint true, got %v", name, ann)
		}
	}
	if ann := byName["search_vector"]["annotations"].(map[string]any); ann["idempotentHint"] != false {
		t.Fatalf("search_vector reaches the embedding provider on every text call and must not claim idempotent, got %v", ann)
	}
	if ann := byName["query"]["annotations"].(map[string]any); ann["destructiveHint"] != false || ann["idempotentHint"] != true {
		t.Fatalf("query hints must be non-destructive and idempotent, got %v", ann)
	}
}

func TestToolsCallReturnsStructuredContent(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	call := func(id int, name string, arguments map[string]any) map[string]any {
		t.Helper()
		code, res := rpc(t, url, map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": arguments},
		})
		if code != 200 {
			t.Fatalf("tools/call %s status %d", name, code)
		}
		return res["result"].(map[string]any)
	}

	callRes := call(1, "create_table", map[string]any{
		"namespace": "agents",
		"table":     "memory",
		"fields":    []map[string]any{{"name": "fact", "type": "string", "fulltext": true}},
	})
	if callRes["isError"] == true {
		t.Fatalf("create_table failed: %v", callRes)
	}
	table, ok := callRes["structuredContent"].(map[string]any)["table"].(map[string]any)
	if !ok {
		t.Fatalf("create_table structuredContent.table must be an object, got %v", callRes["structuredContent"])
	}
	if table["name"] != "memory" || table["namespace"] != "agents" || table["version"].(float64) != 1 {
		t.Fatalf("unexpected table in structuredContent: %v", table)
	}
	if _, ok := table["fields"].([]any); !ok {
		t.Fatalf("table.fields must be an array, got %v", table["fields"])
	}

	callRes = call(2, "insert", map[string]any{
		"namespace": "agents",
		"table":     "memory",
		"records":   []map[string]any{{"fact": "first fact"}, {"fact": "second fact"}},
	})
	inserted, ok := callRes["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("insert must return structuredContent, got %v", callRes)
	}
	ids, _ := inserted["ids"].([]any)
	if len(ids) != 2 || inserted["inserted"].(float64) != 2 {
		t.Fatalf("unexpected insert structuredContent: %v", inserted)
	}
	if content, ok := callRes["content"].([]any); !ok || len(content) != 0 {
		t.Fatalf("successful results must not mirror the payload as text, got %v", callRes["content"])
	}

	callRes = call(3, "list_tables", map[string]any{"namespace": "agents"})
	tables, _ := callRes["structuredContent"].(map[string]any)["tables"].([]any)
	if len(tables) != 1 || tables[0] != "memory" {
		t.Fatalf("unexpected tables in structuredContent: %v", tables)
	}

	callRes = call(4, "query", map[string]any{
		"namespace": "agents",
		"sql":       "SELECT fact FROM memory",
	})
	queried, _ := callRes["structuredContent"].(map[string]any)
	rows, _ := queried["rows"].([]any)
	if queried["row_count"].(float64) != 2 || len(rows) != 2 || queried["truncated"] != false {
		t.Fatalf("unexpected query structuredContent: %v", queried)
	}

	callRes = call(5, "describe_table", map[string]any{"namespace": "agents", "table": "memory"})
	described, _ := callRes["structuredContent"].(map[string]any)
	if described["row_count"].(float64) != 2 || described["table"].(map[string]any)["name"] != "memory" {
		t.Fatalf("unexpected describe_table structuredContent: %v", described)
	}

	callRes = call(6, "search_fulltext", map[string]any{"namespace": "agents", "table": "memory", "query": "first"})
	searched, _ := callRes["structuredContent"].(map[string]any)
	results, _ := searched["results"].([]any)
	if len(results) != 1 || searched["truncated"] != false {
		t.Fatalf("unexpected search_fulltext structuredContent: %v", searched)
	}
	if results[0].(map[string]any)["fact"] != "first fact" {
		t.Fatalf("unexpected match: %v", results[0])
	}
}

func TestToolsCallVectorSearchStructuredContent(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	call := func(id int, name string, arguments map[string]any) map[string]any {
		t.Helper()
		code, res := rpc(t, url, map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": arguments},
		})
		if code != 200 {
			t.Fatalf("tools/call %s status %d", name, code)
		}
		return res["result"].(map[string]any)
	}
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "create_table", "arguments": map[string]any{
			"namespace": "agents",
			"table":     "notes",
			"fields":    []map[string]any{{"name": "body", "type": "text", "vectorize": true}},
		}},
	})
	if code != 200 || res["result"].(map[string]any)["isError"] == true {
		t.Fatalf("create_table failed: %d %v", code, res)
	}
	callRes := call(2, "insert", map[string]any{
		"namespace": "agents",
		"table":     "notes",
		"records":   []map[string]any{{"body": "hello world"}},
	})
	if callRes["isError"] == true {
		t.Fatalf("insert failed: %v", callRes)
	}
	callRes = call(3, "search_vector", map[string]any{
		"namespace": "agents", "table": "notes", "text": "hello", "limit": 5,
	})
	if callRes["isError"] == true {
		t.Fatalf("search_vector failed: %v", callRes)
	}
	results, _ := callRes["structuredContent"].(map[string]any)["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected one hit, got %v", results)
	}
	row := results[0].(map[string]any)
	if _, ok := row["_score"].(float64); !ok {
		t.Fatalf("vector search result must carry a numeric _score, got %v", row)
	}
}

func TestToolsCallErrorOmitsStructuredContent(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "describe_table", "arguments": map[string]any{
			"namespace": "agents", "table": "missing",
		}},
	})
	if code != 200 {
		t.Fatalf("tools/call status %d", code)
	}
	callRes, _ := res["result"].(map[string]any)
	if callRes["isError"] != true {
		t.Fatalf("describe_table on a missing table must be a tool error, got %v", callRes)
	}
	if _, ok := callRes["structuredContent"]; ok {
		t.Fatalf("error results must not carry structuredContent, got %v", callRes)
	}
	text := callRes["content"].([]any)[0].(map[string]any)["text"].(string)
	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("error content is not a valid JSON envelope: %v\ntext: %s", err, text)
	}
	if env["code"] != "not_found" {
		t.Fatalf("error envelope must carry not_found code, got %v", env)
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "missing") {
		t.Fatalf("error message should describe the failure, got %q", msg)
	}
	if strings.Contains(msg, "SQL logic error") {
		t.Fatalf("raw SQLite leaked into MCP tool error: %q", msg)
	}
}

// callTool invokes a stateless tools/call and returns its structuredContent.
func callTool(t *testing.T, url, name string, args map[string]any) map[string]any {
	t.Helper()
	code, res := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	if code != 200 {
		t.Fatalf("tools/call %s status %d: %v", name, code, res)
	}
	result, ok := res["result"].(map[string]any)
	if !ok || result["isError"] == true {
		t.Fatalf("tools/call %s failed: %v", name, res)
	}
	out, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call %s returned no structuredContent: %v", name, result)
	}
	return out
}

func TestMCPTypedReadContract(t *testing.T) {
	url := newMCPServer(t).URL + "/mcp"
	code, _ := rpc(t, url, map[string]any{
		"jsonrpc": "2.0", "id": 0, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
		},
	})
	if code != 200 {
		t.Fatalf("initialize status %d", code)
	}
	rpc(t, url, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	callTool(t, url, "create_table", map[string]any{
		"namespace": "typed",
		"table":     "docs",
		"fields": []map[string]any{
			{"name": "title", "type": "string", "fulltext": true},
			{"name": "body", "type": "text", "vectorize": true},
			{"name": "done", "type": "boolean"},
			{"name": "meta", "type": "json"},
			{"name": "vec", "type": "vector", "dim": 4},
		},
	})
	callTool(t, url, "insert", map[string]any{
		"namespace": "typed",
		"table":     "docs",
		"records": []map[string]any{{
			"title": "typed reads", "body": "the contract holds", "done": true,
			"meta": map[string]any{"k": []any{1, "x"}}, "vec": []any{1.0, 2.0, 3.0, 4.0},
		}},
	})

	assertRow := func(row map[string]any) {
		t.Helper()
		if row["done"] != true {
			t.Fatalf("boolean must arrive as JSON true, got %T %v", row["done"], row["done"])
		}
		meta, ok := row["meta"].(map[string]any)
		if !ok || len(meta["k"].([]any)) != 2 {
			t.Fatalf("json must arrive decoded, got %T %v", row["meta"], row["meta"])
		}
		if vec, ok := row["vec"].([]any); !ok || len(vec) != 4 || vec[0] != float64(1) {
			t.Fatalf("vector must arrive as a number array, got %T %v", row["vec"], row["vec"])
		}
		if _, has := row["_embedding"]; has {
			t.Fatalf("_embedding must stay hidden, got %v", row)
		}
	}

	out := callTool(t, url, "query", map[string]any{"namespace": "typed", "sql": "SELECT * FROM docs"})
	assertRow(out["rows"].([]any)[0].(map[string]any))

	out = callTool(t, url, "search_fulltext", map[string]any{
		"namespace": "typed", "table": "docs", "query": "typed",
	})
	assertRow(out["results"].([]any)[0].(map[string]any))

	out = callTool(t, url, "search_vector", map[string]any{
		"namespace": "typed", "table": "docs", "vector": []float64{1, 0, 0, 0}, "column": "vec",
	})
	results := out["results"].([]any)
	hit := results[0].(map[string]any)
	assertRow(hit)
	if _, ok := hit["_score"].(float64); !ok {
		t.Fatalf("search_vector must attach _score, got %T %v", hit["_score"], hit["_score"])
	}
}

// The wiring mirrors main.go — one api server behind /, one mcp server at
// /mcp — so every version surface must report the single release identity
// from internal/version (issue #67).
func TestVersionSurfacesAgree(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	apiSrv := api.New(st, fakeEmb{})
	mux := http.NewServeMux()
	mux.Handle("/mcp", New(apiSrv, nil))
	mux.Handle("/", apiSrv.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatalf("get /version: %v", err)
	}
	defer res.Body.Close()
	var httpBody map[string]any
	if err := json.NewDecoder(res.Body).Decode(&httpBody); err != nil {
		t.Fatalf("decode /version: %v", err)
	}

	code, rpcBody := rpc(t, srv.URL+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "version-test", "version": "0"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("initialize failed: %d %v", code, rpcBody)
	}
	result, ok := rpcBody["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result missing: %v", rpcBody)
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result missing serverInfo: %v", rpcBody)
	}

	if httpBody["version"] != version.Version || serverInfo["version"] != version.Version {
		t.Fatalf("version surfaces disagree: /version=%v serverInfo=%v want=%s",
			httpBody["version"], serverInfo["version"], version.Version)
	}
}
