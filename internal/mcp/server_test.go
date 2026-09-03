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
	if !strings.Contains(res["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), `"inserted":true`) {
		t.Fatalf("upsert must report an insert, got %v", res)
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
	if !strings.Contains(res["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), `"updated":1`) {
		t.Fatalf("update must report one row, got %v", res)
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
	content := res["result"].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "memory") {
		t.Fatalf("expected memory table in result, got: %s", text)
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

// callTool invokes a stateless tools/call and decodes the JSON text payload.
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
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("tools/call %s result is not JSON: %v", name, err)
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
