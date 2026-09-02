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
		"params": map[string]any{"protocolVersion": "2025-06-18"},
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
		"params": map[string]any{"protocolVersion": "1999-01-01"},
	})
	if code != 200 {
		t.Fatalf("initialize status %d", code)
	}
	if got := res["result"].(map[string]any)["protocolVersion"]; got != "2025-06-18" {
		t.Fatalf("unsupported requested version must fall back to server version, got %v", got)
	}
}
