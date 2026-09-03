package skill

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderSubstitutesTemplateVariables(t *testing.T) {
	ctx := Context{
		BaseURL:       "http://example.com",
		MCPURL:        "http://example.com/mcp",
		Version:       "v0.2.0",
		NamespaceHint: "Use the `team` namespace.",
	}

	for _, name := range []string{"dolmen", "dolmen-admin"} {
		out, err := Render(name, ctx)
		if err != nil {
			t.Fatalf("render %q: %v", name, err)
		}
		body := string(out)
		for _, needle := range []string{ctx.BaseURL, ctx.MCPURL, ctx.Version, ctx.NamespaceHint} {
			if !strings.Contains(body, needle) {
				t.Fatalf("%s: rendered body missing %q:\n%s", name, needle, body)
			}
		}
		if strings.Contains(body, "{{ .BaseURL }}") || strings.Contains(body, "{{ .MCPURL }}") || strings.Contains(body, "{{ .Version }}") || strings.Contains(body, "{{ .NamespaceHint }}") {
			t.Fatalf("%s: template placeholders left in rendered output", name)
		}
	}
}

func TestRenderRejectsUnknownSkill(t *testing.T) {
	ctx := Context{BaseURL: "http://example.com", MCPURL: "http://example.com/mcp", Version: "v0.2.0"}
	if _, err := Render("dolmen-missing", ctx); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}

func TestManifestShape(t *testing.T) {
	ctx := Context{
		BaseURL:       "http://example.com",
		MCPURL:        "http://example.com/mcp",
		Version:       "v0.2.0",
		NamespaceHint: "Use the `team` namespace.",
	}

	raw, err := ManifestJSON(ctx)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v\n%s", err, raw)
	}
	if m.Name != "dolmen" {
		t.Fatalf("manifest name: got %q, want %q", m.Name, "dolmen")
	}
	if m.Version != ctx.Version {
		t.Fatalf("manifest version: got %q, want %q", m.Version, ctx.Version)
	}
	if m.BaseURL != ctx.BaseURL {
		t.Fatalf("manifest base_url: got %q, want %q", m.BaseURL, ctx.BaseURL)
	}
	if m.MCPURL != ctx.MCPURL {
		t.Fatalf("manifest mcp_url: got %q, want %q", m.MCPURL, ctx.MCPURL)
	}
	if m.LayerPicker == "" {
		t.Fatalf("manifest layer_picker must not be empty")
	}
	if len(m.Skills) != 2 {
		t.Fatalf("expected two skills, got %d", len(m.Skills))
	}
	byName := map[string]ManifestSkill{}
	for _, s := range m.Skills {
		byName[s.Name] = s
	}
	for _, want := range []string{"dolmen", "dolmen-admin"} {
		s, ok := byName[want]
		if !ok {
			t.Fatalf("manifest missing skill %q", want)
		}
		if s.Path == "" {
			t.Fatalf("%s: missing path", s.Name)
		}
		if s.Layer == "" {
			t.Fatalf("%s: missing layer", s.Name)
		}
		if s.Audience == "" {
			t.Fatalf("%s: missing audience", s.Name)
		}
	}
	if byName["dolmen"].Layer != "core" {
		t.Fatalf("dolmen layer: got %q, want %q", byName["dolmen"].Layer, "core")
	}
	if byName["dolmen-admin"].Layer != "admin" {
		t.Fatalf("dolmen-admin layer: got %q, want %q", byName["dolmen-admin"].Layer, "admin")
	}
}

func TestMCPInstructionsContainsSkillsURL(t *testing.T) {
	ctx := Context{
		BaseURL:       "http://example.com",
		MCPURL:        "http://example.com/mcp",
		Version:       "v0.2.0",
		NamespaceHint: "Use the `team` namespace.",
	}
	inst := MCPInstructions(ctx)
	if !strings.Contains(inst, ctx.BaseURL+"/skills") {
		t.Fatalf("instructions missing skills URL: %q", inst)
	}
	if !strings.Contains(inst, ctx.MCPURL) {
		t.Fatalf("instructions missing MCP URL: %q", inst)
	}
}

func TestETagIsVersionDerivedAndStable(t *testing.T) {
	body := []byte("skill body")
	etag := ETag("dolmen", "v0.2.0", body)
	if etag == "" || etag == `W/""` {
		t.Fatalf("ETag must not be empty")
	}
	if etag != ETag("dolmen", "v0.2.0", body) {
		t.Fatal("ETag must be deterministic")
	}
	if ETag("dolmen", "v0.2.0", body) == ETag("dolmen-admin", "v0.2.0", body) {
		t.Fatal("ETag must differ by resource name")
	}
	if ETag("dolmen", "v0.2.0", body) == ETag("dolmen", "v0.3.0", body) {
		t.Fatal("ETag must differ by version")
	}
	if ETag("dolmen", "v0.2.0", body) == ETag("dolmen", "v0.2.0", []byte("different")) {
		t.Fatal("ETag must differ by body")
	}
}

func TestBaseURLForParsesForwardedHeaderChains(t *testing.T) {
	for _, tc := range []struct {
		name            string
		host            string
		xForwardedProto string
		xForwardedHost  string
		want            string
	}{
		{
			name:            "single forwarded proto and host",
			host:            "127.0.0.1:8080",
			xForwardedProto: "https",
			xForwardedHost:  "public.example.com",
			want:            "https://public.example.com",
		},
		{
			name:            "multi-hop proto and host chain",
			host:            "127.0.0.1:8080",
			xForwardedProto: "https, http",
			xForwardedHost:  "public.example.com, internal.example.com",
			want:            "https://public.example.com",
		},
		{
			name:            "tls fallback when no forwarded proto",
			host:            "example.com",
			xForwardedProto: "",
			xForwardedHost:  "",
			want:            "https://example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/skills", nil)
			r.Host = tc.host
			if tc.xForwardedProto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xForwardedProto)
			}
			if tc.xForwardedHost != "" {
				r.Header.Set("X-Forwarded-Host", tc.xForwardedHost)
			}
			if tc.name == "tls fallback when no forwarded proto" {
				r.TLS = &tls.ConnectionState{}
			}
			if got := BaseURLFor(r, ""); got != tc.want {
				t.Fatalf("BaseURLFor: got %q, want %q", got, tc.want)
			}
		})
	}
}
