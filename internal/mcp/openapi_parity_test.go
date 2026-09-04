package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/store"
)

// newDualServer returns a test server that exposes both the HTTP /v1 API and
// the /mcp JSON-RPC endpoint on the same port. This lets a single test drive
// the same operation through both transports and compare the results.
func newDualServer(t *testing.T) (srv *httptest.Server, apiURL, mcpURL string) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	a := api.New(st, fakeEmb{})
	m := New(a, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", m)
	mux.Handle("/", a.Handler())

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, srv.URL + "/v1", srv.URL + "/mcp"
}

func postJSON(t *testing.T, url string, body any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode response from %s: %v", url, err)
	}
	return out
}

// jsonNormalize round-trips a value through JSON so maps, slices and scalar
// types match what an HTTP/MCP client sees after encoding and decoding.
func jsonNormalize(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func schemasEqual(t *testing.T, a, b any) bool {
	t.Helper()
	return reflect.DeepEqual(jsonNormalize(t, a), jsonNormalize(t, b))
}

func toolNames(tools []any) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		t := tool.(map[string]any)
		names = append(names, t["name"].(string))
	}
	sort.Strings(names)
	return names
}

// TestMCPInputSchemasMatchOpenAPIRequestSchemas validates that the MCP
// tools/list inputSchema for every operation is exactly the same as the
// /v1/openapi.json requestBody schema for the same operation. This is the
// guard that prevents the two surfaces from drifting silently again.
func TestMCPInputSchemasMatchOpenAPIRequestSchemas(t *testing.T) {
	srv, _, mcpURL := newDualServer(t)

	// Fetch the OpenAPI document over HTTP.
	res, err := http.Get(srv.URL + "/v1/openapi.json")
	if err != nil {
		t.Fatalf("get openapi: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("openapi status %d", res.StatusCode)
	}
	var openAPIDoc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&openAPIDoc); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}

	paths, ok := openAPIDoc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("openapi paths missing")
	}

	// Fetch the MCP tools manifest.
	code, mcpRes := rpc(t, mcpURL, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if code != http.StatusOK {
		t.Fatalf("tools/list status %d", code)
	}
	tools, ok := mcpRes["result"].(map[string]any)["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list result missing tools")
	}

	if len(tools) != len(api.Ops) {
		t.Fatalf("expected %d tools, got %d", len(api.Ops), len(tools))
	}

	byName := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		t := tool.(map[string]any)
		byName[t["name"].(string)] = t
	}

	for _, name := range api.OpNames() {
		pathItem, ok := paths["/v1/"+name].(map[string]any)
		if !ok {
			t.Fatalf("missing OpenAPI path /v1/%s", name)
		}
		post, ok := pathItem["post"].(map[string]any)
		if !ok {
			t.Fatalf("missing post for /v1/%s", name)
		}
		reqBody, ok := post["requestBody"].(map[string]any)
		if !ok {
			t.Fatalf("missing requestBody for /v1/%s", name)
		}
		content, ok := reqBody["content"].(map[string]any)
		if !ok {
			t.Fatalf("missing content for /v1/%s", name)
		}
		jsonContent, ok := content["application/json"].(map[string]any)
		if !ok {
			t.Fatalf("missing application/json for /v1/%s", name)
		}
		openAPISchema, ok := jsonContent["schema"].(map[string]any)
		if !ok {
			t.Fatalf("missing schema for /v1/%s", name)
		}

		tool, ok := byName[name]
		if !ok {
			t.Fatalf("missing MCP tool %s", name)
		}
		mcpSchema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("MCP tool %s has no inputSchema", name)
		}

		if !schemasEqual(t, openAPISchema, mcpSchema) {
			t.Fatalf("MCP inputSchema for %s does not match /v1/openapi.json request schema", name)
		}
	}

	gotNames := toolNames(tools)
	wantNames := api.OpNames()
	sort.Strings(wantNames)
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("tool names mismatch: got %v, want %v", gotNames, wantNames)
	}
}

// TestMigrateAddFieldDefaultMatchesHTTPAndMCP sends the same add_field-with-
// default payload through /v1 and tools/call and verifies both produce the same
// migrated table and backfill. This is the functional acceptance for issue #119.
func TestMigrateAddFieldDefaultMatchesHTTPAndMCP(t *testing.T) {
	_, apiURL, mcpURL := newDualServer(t)

	// Set up an identical table on both sides using the HTTP API.
	createHTTP := map[string]any{
		"namespace": "p",
		"table":     "http",
		"fields":    []map[string]any{{"name": "title", "type": "string"}},
	}
	if out := postJSON(t, apiURL+"/create_table", createHTTP); !out["ok"].(bool) {
		t.Fatalf("create_table over HTTP failed: %v", out)
	}
	if out := postJSON(t, apiURL+"/insert", map[string]any{
		"namespace": "p", "table": "http",
		"records": []map[string]any{{"title": "x"}},
	}); !out["ok"].(bool) {
		t.Fatalf("insert over HTTP failed: %v", out)
	}

	createMCP := map[string]any{
		"namespace": "p",
		"table":     "mcp",
		"fields":    []map[string]any{{"name": "title", "type": "string"}},
	}
	if out := postJSON(t, apiURL+"/create_table", createMCP); !out["ok"].(bool) {
		t.Fatalf("create_table over HTTP (mcp side) failed: %v", out)
	}
	if out := postJSON(t, apiURL+"/insert", map[string]any{
		"namespace": "p", "table": "mcp",
		"records": []map[string]any{{"title": "y"}},
	}); !out["ok"].(bool) {
		t.Fatalf("insert over HTTP (mcp side) failed: %v", out)
	}

	migrateHTTP := map[string]any{
		"namespace": "p",
		"table":     "http",
		"changes": []map[string]any{{
			"op":      "add_field",
			"field":   map[string]any{"name": "status", "type": "string", "required": true},
			"default": "open",
		}},
	}
	httpRes := postJSON(t, apiURL+"/migrate", migrateHTTP)
	if !httpRes["ok"].(bool) {
		t.Fatalf("migrate over HTTP failed: %v", httpRes)
	}
	httpTable := httpRes["data"].(map[string]any)["table"].(map[string]any)

	migrateMCP := map[string]any{
		"namespace": "p",
		"table":     "mcp",
		"changes": []map[string]any{{
			"op":      "add_field",
			"field":   map[string]any{"name": "status", "type": "string", "required": true},
			"default": "open",
		}},
	}
	code, mcpRes := rpc(t, mcpURL, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "migrate", "arguments": migrateMCP},
	})
	if code != http.StatusOK {
		t.Fatalf("tools/call status %d", code)
	}
	result := mcpRes["result"].(map[string]any)
	if result["isError"].(bool) {
		t.Fatalf("migrate over MCP failed: %v", result)
	}
	mcpTable := result["structuredContent"].(map[string]any)["table"].(map[string]any)

	if !schemasEqual(t, httpTable["fields"], mcpTable["fields"]) {
		t.Fatalf("migrated table fields over HTTP and MCP differ:\nHTTP: %v\nMCP: %v", httpTable["fields"], mcpTable["fields"])
	}

	// Both tables should now have version 2 and the new required field.
	if httpTable["version"].(float64) != 2 || mcpTable["version"].(float64) != 2 {
		t.Fatalf("expected version 2, got HTTP %v and MCP %v", httpTable["version"], mcpTable["version"])
	}

	// Verify the default was actually backfilled into the existing rows.
	httpQuery := postJSON(t, apiURL+"/query", map[string]any{
		"namespace": "p", "sql": "SELECT status FROM http WHERE title = 'x'",
	})
	if !httpQuery["ok"].(bool) {
		t.Fatalf("HTTP query failed: %v", httpQuery)
	}
	httpRows := httpQuery["data"].(map[string]any)["rows"].([]any)
	if len(httpRows) != 1 || httpRows[0].(map[string]any)["status"] != "open" {
		t.Fatalf("HTTP backfill failed: %v", httpRows)
	}

	mcpQuery := map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name": "query",
			"arguments": map[string]any{
				"namespace": "p", "sql": "SELECT status FROM mcp WHERE title = 'y'",
			},
		},
	}
	code, mcpQueryRes := rpc(t, mcpURL, mcpQuery)
	if code != http.StatusOK {
		t.Fatalf("tools/call query status %d", code)
	}
	queryResult := mcpQueryRes["result"].(map[string]any)
	if queryResult["isError"].(bool) {
		t.Fatalf("query over MCP failed: %v", queryResult)
	}
	mcpRows := queryResult["structuredContent"].(map[string]any)["rows"].([]any)
	if len(mcpRows) != 1 || mcpRows[0].(map[string]any)["status"] != "open" {
		t.Fatalf("MCP backfill failed: %v", mcpRows)
	}

	// Finally, the change object in the migrate inputSchema must still advertise
	// the default property (the schema-level guard for the bug in #119).
	migrateSchema := api.Ops["migrate"].InputSchema["properties"].(map[string]any)["changes"].(map[string]any)["items"].(map[string]any)
	changeProps := migrateSchema["properties"].(map[string]any)
	if _, ok := changeProps["default"]; !ok {
		t.Fatalf("migrate change inputSchema must declare a default property")
	}
	if _, ok := changeProps["default"].(map[string]any)["description"]; !ok {
		t.Fatalf("migrate change default must be documented")
	}

}
