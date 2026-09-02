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
	srv := httptest.NewServer(New(api.New(st, fakeEmb{})))
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
	if len(tools) != 10 {
		t.Fatalf("expected 10 tools, got %d", len(tools))
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
	}
}
