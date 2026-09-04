package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/mcp"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/version"
	"github.com/lsm/dolmen/skill"
)

func TestVersionFlagPrintsInjectedVersion(t *testing.T) {
	oldArgs, oldStdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldStdout }()
	os.Args = []string{"dolmen", "-version"}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	runErr := run()
	w.Close()
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("run -version: %v", runErr)
	}
	if got, want := strings.TrimSpace(string(out)), "dolmen "+version.Version; got != want {
		t.Fatalf("--version printed %q, want %q", got, want)
	}
}

func TestLoadConfig(t *testing.T) {
	oldREMBED, hadREMBED := os.LookupEnv("REMBED_CACHE")
	t.Cleanup(func() {
		if hadREMBED {
			os.Setenv("REMBED_CACHE", oldREMBED)
		} else {
			os.Unsetenv("REMBED_CACHE")
		}
	})
	os.Unsetenv("REMBED_CACHE")

	dataDir := t.TempDir()

	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		want    *config
		wantErr string
	}{
		{
			name: "defaults",
			args: []string{},
			env:  map[string]string{"DOLMEN_DATA": dataDir},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            dataDir,
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "local"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
			},
		},
		{
			name: "flags override env and defaults",
			args: []string{"-addr", ":8080", "-data", dataDir},
			env:  map[string]string{"DOLMEN_ADDR": ":9999", "DOLMEN_DATA": dataDir + "-wrong"},
			want: &config{
				Addr:               ":8080",
				DataDir:            dataDir,
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "local"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
			},
		},
		{
			name: "prefix flag",
			args: []string{"-prefix", "dolmen/"},
			env:  map[string]string{},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				BaseURL:            "",
				Prefix:             "/dolmen",
				Embed:              embedConfig{Provider: "local"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
			},
		},
		{
			name: "prefix env",
			args: []string{},
			env:  map[string]string{"DOLMEN_PREFIX": "/dolmen"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				BaseURL:            "",
				Prefix:             "/dolmen",
				Embed:              embedConfig{Provider: "local"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
			},
		},
		{
			name:    "base-url that already ends with prefix is rejected",
			args:    []string{"-base-url", "https://example.com/dolmen", "-prefix", "/dolmen"},
			env:     map[string]string{},
			wantErr: "already ends with -prefix",
		},
		{
			name:    "base-url host part plus prefix is allowed",
			args:    []string{"-base-url", "https://example.com", "-prefix", "/dolmen"},
			env:     map[string]string{},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				BaseURL:            "https://example.com",
				Prefix:             "/dolmen",
				Embed:              embedConfig{Provider: "local"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
			},
		},
		{
			name: "env fills config",
			args: []string{},
			env: map[string]string{
				"DOLMEN_ADDR":            ":9000",
				"DOLMEN_DATA":            "/data",
				"DOLMEN_ALLOWED_ORIGINS": "http://a , http://b",
				"DOLMEN_EMBED_PROVIDER":  "openai",
				"DOLMEN_EMBED_BASE_URL":  "http://localhost:11434/v1",
				"DOLMEN_EMBED_MODEL":     "nomic-embed-text",
				"DOLMEN_EMBED_API_KEY":   "secret",
			},
			want: &config{
				Addr:           ":9000",
				DataDir:        "/data",
				AllowedOrigins: []string{"http://a", "http://b"},
				Embed: embedConfig{
					Provider: "openai",
					BaseURL:  "http://localhost:11434/v1",
					Model:    "nomic-embed-text",
					APIKey:   "secret",
				},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
			},
		},
		{
			name: "openai api key falls back to OPENAI_API_KEY",
			args: []string{},
			env: map[string]string{
				"DOLMEN_EMBED_PROVIDER": "openai",
				"OPENAI_API_KEY":        "fallback",
			},
			want: &config{
				Addr:    "127.0.0.1:8790",
				DataDir: "data",
				Embed: embedConfig{
					Provider: "openai",
					APIKey:   "fallback",
				},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
			},
		},
		{
			name: "explicit empty DOLMEN_EMBED_API_KEY suppresses fallback",
			args: []string{},
			env: map[string]string{
				"DOLMEN_EMBED_PROVIDER": "openai",
				"DOLMEN_EMBED_API_KEY":  "",
				"OPENAI_API_KEY":        "fallback",
			},
			want: &config{
				Addr:    "127.0.0.1:8790",
				DataDir: "data",
				Embed: embedConfig{
					Provider: "openai",
					APIKey:   "",
				},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
			},
		},
		{
			name:    "unknown embed provider is rejected",
			args:    []string{},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "foo"},
			wantErr: "unknown embedding provider",
		},
		{
			name: "local provider with an invalid model is rejected",
			args: []string{},
			env: map[string]string{
				"DOLMEN_EMBED_PROVIDER": "local",
				"DOLMEN_EMBED_MODEL":    "not a model",
			},
			wantErr: "neither a Hugging Face model id",
		},
		{
			name:    "positional arguments are rejected",
			args:    []string{"extra"},
			env:     map[string]string{},
			wantErr: "unexpected positional argument",
		},
		{
			name:    "unknown flags are rejected",
			args:    []string{"-unknown"},
			env:     map[string]string{},
			wantErr: "flag provided but not defined",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				return tc.env[key]
			}
			lookupEnv := func(key string) (string, bool) {
				v, ok := tc.env[key]
				return v, ok
			}
			cfg, err := loadConfig(tc.args, getenv, lookupEnv, io.Discard)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(cfg, tc.want) {
				t.Fatalf("got %+v, want %+v", cfg, tc.want)
			}
		})
	}
}

func TestLoadConfigVersion(t *testing.T) {
	cfg, err := loadConfig([]string{"-version"}, func(string) string { return "" }, func(string) (string, bool) { return "", false }, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Version {
		t.Fatalf("expected Version to be true")
	}
}

// TestLoadConfigLocalProvider covers the local provider's config path
// outside the table: a valid model passes validation and points the model
// cache at <data>/models. It needs a real temp data dir because the local
// provider creates the cache directory at config time.
func TestLoadConfigLocalProvider(t *testing.T) {
	old, had := os.LookupEnv("REMBED_CACHE")
	t.Cleanup(func() {
		if had {
			os.Setenv("REMBED_CACHE", old)
		} else {
			os.Unsetenv("REMBED_CACHE")
		}
	})
	os.Unsetenv("REMBED_CACHE")

	dataDir := t.TempDir()
	env := map[string]string{
		"DOLMEN_EMBED_PROVIDER": "local",
		"DOLMEN_DATA":           dataDir,
	}
	cfg, err := loadConfig([]string{}, func(k string) string { return env[k] },
		func(k string) (string, bool) { v, ok := env[k]; return v, ok }, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Embed.Provider != "local" {
		t.Fatalf("provider: %q", cfg.Embed.Provider)
	}
	// The model default kicks in inside NewProvider; config records the
	// raw empty value, the provider owns the default.
	if cfg.Embed.Model != "" {
		t.Fatalf("model: %q", cfg.Embed.Model)
	}
	if got := os.Getenv("REMBED_CACHE"); got != filepath.Join(dataDir, "models") {
		t.Fatalf("REMBED_CACHE: %q", got)
	}
}

func TestPrefixRouting(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	apiSrv := api.New(st, embed.None{}, api.WithPrefix("/dolmen"))
	mcpSrv := mcp.New(apiSrv, nil, mcp.WithPrefix("/dolmen"))

	sub := http.NewServeMux()
	sub.Handle("/mcp", mcpSrv)
	sub.Handle("/", apiSrv.Handler())
	srv := httptest.NewServer(withPrefix("/dolmen", sub))
	t.Cleanup(srv.Close)

	// Paths without the prefix must 404.
	for _, path := range []string{"/healthz", "/version", "/skills", "/mcp", "/dolmenfoo"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: expected 404, got %d", path, res.StatusCode)
		}
	}

	// Prefixed API endpoints must work.
	res, err := http.Get(srv.URL + "/dolmen/healthz")
	if err != nil {
		t.Fatalf("get /dolmen/healthz: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/dolmen/healthz: got %d", res.StatusCode)
	}

	res, err = http.Get(srv.URL + "/dolmen/version")
	if err != nil {
		t.Fatalf("get /dolmen/version: %v", err)
	}
	var versionBody map[string]any
	if err := json.NewDecoder(res.Body).Decode(&versionBody); err != nil {
		t.Fatalf("decode /dolmen/version: %v", err)
	}
	res.Body.Close()
	if versionBody["name"] != "dolmen" {
		t.Fatalf("version name: %v", versionBody["name"])
	}

	res, err = http.Get(srv.URL + "/dolmen/skills")
	if err != nil {
		t.Fatalf("get /dolmen/skills: %v", err)
	}
	var manifest skill.Manifest
	if err := json.NewDecoder(res.Body).Decode(&manifest); err != nil {
		t.Fatalf("decode /dolmen/skills: %v", err)
	}
	res.Body.Close()
	wantBase := srv.URL + "/dolmen"
	if manifest.BaseURL != wantBase {
		t.Fatalf("manifest base_url: got %q, want %q", manifest.BaseURL, wantBase)
	}
	if manifest.MCPURL != wantBase+"/mcp" {
		t.Fatalf("manifest mcp_url: got %q, want %q", manifest.MCPURL, wantBase+"/mcp")
	}
	if manifest.OpenAPIURL != wantBase+"/v1/openapi.json" {
		t.Fatalf("manifest openapi_url: got %q, want %q", manifest.OpenAPIURL, wantBase+"/v1/openapi.json")
	}

	// Prefixed MCP endpoint must work.
	initBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
		},
	})
	res, err = http.Post(srv.URL+"/dolmen/mcp", "application/json", bytes.NewReader(initBody))
	if err != nil {
		t.Fatalf("post /dolmen/mcp: %v", err)
	}
	var rpc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&rpc); err != nil {
		t.Fatalf("decode /dolmen/mcp: %v", err)
	}
	res.Body.Close()
	result, ok := rpc["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing initialize result: %v", rpc)
	}
	instructions, _ := result["instructions"].(string)
	if !strings.Contains(instructions, wantBase+"/skills") {
		t.Fatalf("instructions missing skills URL: %q", instructions)
	}
	if !strings.Contains(instructions, wantBase+"/mcp") {
		t.Fatalf("instructions missing MCP URL: %q", instructions)
	}

	// A request with a URL-encoded path component after the prefix must not match.
	res, err = http.Get(srv.URL + "/dolmen%2ffoo")
	if err != nil {
		t.Fatalf("get /dolmen%%2ffoo: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("/dolmen%%2ffoo: expected 404, got %d", res.StatusCode)
	}
}

func TestPrefixRoutingWithoutPrefix(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	apiSrv := api.New(st, embed.None{})
	mcpSrv := mcp.New(apiSrv, nil)
	sub := http.NewServeMux()
	sub.Handle("/mcp", mcpSrv)
	sub.Handle("/", apiSrv.Handler())
	srv := httptest.NewServer(withPrefix("", sub))
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get /healthz: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/healthz: got %d", res.StatusCode)
	}
}
