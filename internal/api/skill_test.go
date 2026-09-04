package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/version"
	"github.com/lsm/dolmen/skill"
)

func get(t *testing.T, base, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return res
}

func TestSkillsManifest(t *testing.T) {
	srv := newTestServer(t)
	res := get(t, srv.URL, "/skills", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /skills: got %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("manifest content-type: got %q", ct)
	}
	manifestBody, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("manifest must carry an ETag")
	}
	wantETag := skill.ETag("manifest", version.Version, manifestBody)
	if etag != wantETag {
		t.Fatalf("ETag: got %q, want %q", etag, wantETag)
	}

	var m skill.Manifest
	if err := json.Unmarshal(manifestBody, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.Name != "dolmen" {
		t.Fatalf("manifest name: got %q", m.Name)
	}
	if m.Version != version.Version {
		t.Fatalf("manifest version: got %q, want %q", m.Version, version.Version)
	}
	if m.BaseURL != srv.URL {
		t.Fatalf("manifest base_url: got %q, want %q", m.BaseURL, srv.URL)
	}
	if m.MCPURL != srv.URL+"/mcp" {
		t.Fatalf("manifest mcp_url: got %q, want %q", m.MCPURL, srv.URL+"/mcp")
	}
	if m.OpenAPIURL != srv.URL+"/v1/openapi.json" {
		t.Fatalf("manifest openapi_url: got %q, want %q", m.OpenAPIURL, srv.URL+"/v1/openapi.json")
	}
	if m.LayerPicker == "" {
		t.Fatal("manifest layer_picker must not be empty")
	}
	if len(m.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(m.Skills))
	}

	// 304 on matching If-None-Match.
	res2 := get(t, srv.URL, "/skills", map[string]string{"If-None-Match": etag})
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match: got %d, want 304", res2.StatusCode)
	}

	// 304 on wildcard If-None-Match.
	res3 := get(t, srv.URL, "/skills/dolmen", map[string]string{"If-None-Match": "*"})
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match *: got %d, want 304", res3.StatusCode)
	}
	body, _ := io.ReadAll(res2.Body)
	if len(body) != 0 {
		t.Fatalf("304 response must be empty, got %d bytes", len(body))
	}
}

func TestSkillMarkdown(t *testing.T) {
	srv := newTestServer(t)
	res := get(t, srv.URL, "/skills/dolmen", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /skills/dolmen: got %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
		t.Fatalf("skill content-type: got %q", ct)
	}

	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	wantETag := skill.ETag("dolmen", version.Version, body)
	if got := res.Header.Get("ETag"); got != wantETag {
		t.Fatalf("skill ETag: got %q, want %q", got, wantETag)
	}

	if !strings.Contains(string(body), srv.URL+"/mcp") {
		t.Fatalf("rendered skill missing MCP URL")
	}
	if !strings.Contains(string(body), skill.DefaultNamespaceHint) {
		t.Fatalf("rendered skill missing default namespace hint")
	}
	if strings.Contains(string(body), "{{ .BaseURL }}") {
		t.Fatal("template placeholders left in rendered skill")
	}

	res2 := get(t, srv.URL, "/skills/dolmen", map[string]string{"If-None-Match": "W/" + res.Header.Get("ETag")})
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match with W/ prefix: got %d, want 304", res2.StatusCode)
	}
}

func TestSkillUnknownSkillIs404(t *testing.T) {
	srv := newTestServer(t)
	res := get(t, srv.URL, "/skills/not-a-skill", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown skill: got %d, want 404", res.StatusCode)
	}
}

func TestSkillMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Post(srv.URL+"/skills", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /skills: got %d, want 405", res.StatusCode)
	}
	if allow := res.Header.Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow header: got %q, want %q", allow, http.MethodGet)
	}
}

func TestSkillBaseURLFromForwardedPrefix(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := httptest.NewServer(New(st, fakeEmb{}).Handler())
	defer srv.Close()

	res := get(t, srv.URL, "/skills", map[string]string{
		"X-Forwarded-Proto":  "https",
		"X-Forwarded-Host":   "public.example.com",
		"X-Forwarded-Prefix": "/dolmen/",
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /skills: got %d", res.StatusCode)
	}
	var m skill.Manifest
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.BaseURL != "https://public.example.com/dolmen" {
		t.Fatalf("manifest base_url: got %q", m.BaseURL)
	}
	if m.MCPURL != "https://public.example.com/dolmen/mcp" {
		t.Fatalf("manifest mcp_url: got %q", m.MCPURL)
	}
	if m.OpenAPIURL != "https://public.example.com/dolmen/v1/openapi.json" {
		t.Fatalf("manifest openapi_url: got %q", m.OpenAPIURL)
	}
}

func TestSkillBaseURLFromConfiguredValue(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := httptest.NewServer(New(st, fakeEmb{}, WithBaseURL("https://dolmen.example.com")).Handler())
	defer srv.Close()

	res := get(t, srv.URL, "/skills", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /skills: got %d", res.StatusCode)
	}
	var m skill.Manifest
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.BaseURL != "https://dolmen.example.com" {
		t.Fatalf("manifest base_url: got %q", m.BaseURL)
	}
	if m.MCPURL != "https://dolmen.example.com/mcp" {
		t.Fatalf("manifest mcp_url: got %q", m.MCPURL)
	}
}
