package blackbox

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestStage01BootAndDiscover(t *testing.T) {
	resp, err := httpClient.Get(app.srv.url + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body, healthErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if healthErr != nil {
		t.Fatalf("read /healthz: %v", healthErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz: HTTP %d: %s", resp.StatusCode, body)
	}
	if strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Fatalf("/healthz body: %s", body)
	}

	resp, err = httpClient.Get(app.srv.url + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/version: HTTP %d", resp.StatusCode)
	}
	var version struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	decodeInto(t, body, &version, "version")
	if version.Name != "dolmen" || version.Version == "" {
		t.Fatalf("/version body: %s", body)
	}

	resp, err = httpClient.Get(app.srv.url + "/skills")
	if err != nil {
		t.Fatalf("GET /skills: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/skills: HTTP %d", resp.StatusCode)
	}
	var manifest struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		MCPURL  string `json:"mcp_url"`
		Skills  []struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			Layer string `json:"layer"`
		} `json:"skills"`
	}
	decodeInto(t, body, &manifest, "skills manifest")
	if manifest.Name != "dolmen" {
		t.Fatalf("/skills manifest name: %q", manifest.Name)
	}
	if manifest.BaseURL != app.srv.url {
		t.Fatalf("manifest base_url %q does not match the -base-url the server was started with", manifest.BaseURL)
	}
	if manifest.MCPURL != app.srv.url+"/mcp" {
		t.Fatalf("manifest mcp_url %q does not match the running endpoint", manifest.MCPURL)
	}
	paths := map[string]string{}
	for _, s := range manifest.Skills {
		paths[s.Name] = s.Path
	}
	for _, want := range []string{"dolmen", "dolmen-admin"} {
		path, ok := paths[want]
		if !ok {
			t.Fatalf("skills manifest does not name the %s skill: %s", want, body)
		}
		resp, err := httpClient.Get(app.srv.url + path)
		if err != nil {
			t.Fatalf("GET the %s skill: %v", want, err)
		}
		md, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s skill: HTTP %d", want, resp.StatusCode)
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
			t.Fatalf("%s skill content type %q is not markdown", want, resp.Header.Get("Content-Type"))
		}
		if !strings.Contains(string(md), "name: "+want) {
			t.Fatalf("%s skill markdown does not carry its own name in the frontmatter", want)
		}
	}

	openAPITestPaths(t)

	tableSchema := openapiSchema(t, "TableSchema")
	sample := map[string]any{
		"namespace": "acme/support",
		"name":      "tickets",
		"version":   float64(1),
		"fields":    []any{map[string]any{"name": "subject", "type": "string"}},
	}
	assertConforms(t, tableSchema, sample, "TableSchema.self-check")
	badSample := map[string]any{
		"namespace": "acme/support",
		"name":      "tickets",
		"fields":    []any{map[string]any{"name": "subject", "type": "string"}},
		"rogue":     true,
	}
	if problems := conformanceProblems(t, tableSchema, badSample, "TableSchema.negative-check"); len(problems) < 2 {
		t.Fatalf("the schema validator did not reject a table object missing version and carrying a rogue key: %v", problems)
	}

	errorSchema := openapiSchema(t, "ErrorEnvelope")
	props, _ := errorSchema["properties"].(map[string]any)
	errProp, _ := props["error"].(map[string]any)
	errProps, _ := errProp["properties"].(map[string]any)
	codeProp, _ := errProps["code"].(map[string]any)
	enum, _ := codeProp["enum"].([]any)
	served := map[string]bool{}
	for _, c := range enum {
		served[asStr(t, c, "ErrorEnvelope error.code enum entry")] = true
	}
	for _, documented := range []string{"invalid_request", "not_found", "query_error", "conflict", "forbidden", "embedder_unavailable", "internal_error"} {
		if !served[documented] {
			t.Errorf("served ErrorEnvelope code enum is missing the documented code %q", documented)
		}
	}
}

func openAPITestPaths(t *testing.T) {
	t.Helper()
	paths := openapiPaths(t)
	needed := []string{
		"create_namespace", "create_table", "describe_table", "list_tables",
		"insert", "search_fulltext", "search_vector", "upsert_by_key",
		"migrate", "changes_since", "wait_for", "query",
	}
	for _, opName := range needed {
		if _, ok := paths["/v1/"+opName]; !ok {
			t.Errorf("served openapi is missing /v1/%s", opName)
		}
	}
}
