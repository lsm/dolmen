package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/auth"
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
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
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
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
			},
		},
		{
			name: "engine flag overrides env",
			args: []string{"-engine", "sqlite"},
			env:  map[string]string{"DOLMEN_ENGINE": "postgres", "DOLMEN_EMBED_PROVIDER": "none"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				Engine:             "sqlite",
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
			},
		},
		{
			name: "engine env accepted",
			args: []string{},
			env:  map[string]string{"DOLMEN_ENGINE": "sqlite", "DOLMEN_EMBED_PROVIDER": "none"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				Engine:             "sqlite",
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
			},
		},
		{
			name:    "unknown engine teaches the available engine",
			args:    []string{},
			env:     map[string]string{"DOLMEN_ENGINE": "postgres", "DOLMEN_EMBED_PROVIDER": "none"},
			wantErr: `unknown engine "postgres" (the available engine is "sqlite")`,
		},
		{
			name: "prefix flag",
			args: []string{"-prefix", "dolmen/"},

			env: map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				BaseURL:            "",
				Prefix:             "/dolmen",
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
			},
		},
		{
			name: "prefix env",
			args: []string{},
			env:  map[string]string{"DOLMEN_PREFIX": "/dolmen", "DOLMEN_EMBED_PROVIDER": "none"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				BaseURL:            "",
				Prefix:             "/dolmen",
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
			},
		},
		{
			name:    "base-url that already ends with prefix is rejected",
			args:    []string{"-base-url", "https://example.com/dolmen", "-prefix", "/dolmen"},
			env:     map[string]string{},
			wantErr: "already ends with -prefix",
		},
		{
			name: "base-url host part plus prefix is allowed",
			args: []string{"-base-url", "https://example.com", "-prefix", "/dolmen"},
			env:  map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				BaseURL:            "https://example.com",
				Prefix:             "/dolmen",
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
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
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
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
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
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
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
			},
		},
		{
			name:    "unknown embed provider is rejected",
			args:    []string{},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "foo"},
			wantErr: "unknown embedding provider",
		},
		{
			name: "change retention zero disables pruning",
			args: []string{"-change-retention", "0"},
			env:  map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    0,
				MaxSubscriptionAge: 30 * time.Minute,
			},
		},
		{
			name: "change retention from the environment",
			args: []string{},
			env:  map[string]string{"DOLMEN_EMBED_PROVIDER": "none", "DOLMEN_CHANGE_RETENTION": "48h"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    48 * time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
			},
		},
		{
			name: "change retention bounds are inclusive",
			args: []string{"-change-retention", "1h"},
			env:  map[string]string{"DOLMEN_EMBED_PROVIDER": "none", "DOLMEN_CHANGE_RETENTION": "2160h"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    time.Hour,
				MaxSubscriptionAge: 30 * time.Minute,
			},
		},
		{
			name:    "change retention below the floor is rejected",
			args:    []string{"-change-retention", "30m"},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			wantErr: "must be 0 (disable pruning) or between 1h and 2160h",
		},
		{
			name:    "change retention above the ceiling is rejected",
			args:    []string{"-change-retention", "2200h"},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			wantErr: "must be 0 (disable pruning) or between 1h and 2160h",
		},
		{
			name:    "negative change retention is rejected",
			args:    []string{"-change-retention", "-1h"},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			wantErr: "must be 0 (disable pruning) or between 1h and 2160h",
		},
		{
			name:    "unparseable change retention is rejected",
			args:    []string{"-change-retention", "forever"},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			wantErr: `invalid change retention "forever"`,
		},
		{
			name:    "invalid change retention from the environment is rejected",
			args:    []string{},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none", "DOLMEN_CHANGE_RETENTION": "45m"},
			wantErr: "must be 0 (disable pruning) or between 1h and 2160h",
		},
		{
			name: "max subscription age zero disables the bound",
			args: []string{"-max-subscription-age", "0"},
			env:  map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 0,
			},
		},
		{
			name: "max subscription age from the environment",
			args: []string{},
			env:  map[string]string{"DOLMEN_EMBED_PROVIDER": "none", "DOLMEN_MAX_SUBSCRIPTION_AGE": "5m"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 5 * time.Minute,
			},
		},
		{
			name: "max subscription age bounds are inclusive",
			args: []string{"-max-subscription-age", "24h"},
			env:  map[string]string{"DOLMEN_EMBED_PROVIDER": "none", "DOLMEN_MAX_SUBSCRIPTION_AGE": "1s"},
			want: &config{
				Addr:               "127.0.0.1:8790",
				DataDir:            "data",
				AllowedOrigins:     nil,
				Embed:              embedConfig{Provider: "none"},
				SkillNamespaceHint: skill.DefaultNamespaceHint,
				ChangeRetention:    168 * time.Hour,
				MaxSubscriptionAge: 24 * time.Hour,
			},
		},
		{
			name:    "max subscription age below the floor is rejected",
			args:    []string{"-max-subscription-age", "500ms"},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			wantErr: "must be 0 (disable the bound) or between 1s and 24h",
		},
		{
			name:    "max subscription age above the ceiling is rejected",
			args:    []string{"-max-subscription-age", "25h"},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			wantErr: "must be 0 (disable the bound) or between 1s and 24h",
		},
		{
			name:    "negative max subscription age is rejected",
			args:    []string{"-max-subscription-age", "-1s"},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			wantErr: "must be 0 (disable the bound) or between 1s and 24h",
		},
		{
			name:    "unparseable max subscription age is rejected",
			args:    []string{"-max-subscription-age", "soon"},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none"},
			wantErr: `invalid max subscription age "soon"`,
		},
		{
			name:    "invalid max subscription age from the environment is rejected",
			args:    []string{},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "none", "DOLMEN_MAX_SUBSCRIPTION_AGE": "10ms"},
			wantErr: "must be 0 (disable the bound) or between 1s and 24h",
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
			cfg, err := loadConfig(tc.args, getenv, lookupEnv, io.Discard, false)
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
			if cfg.Auth == nil {
				t.Fatal("loadConfig returned no authenticator")
			}
			if cfg.Auth.On() {
				t.Fatalf("auth is on without DOLMEN_AUTH=on")
			}
			cfg.Auth = nil
			if !reflect.DeepEqual(cfg, tc.want) {
				t.Fatalf("got %+v, want %+v", cfg, tc.want)
			}
		})
	}
}

func TestLoadConfigVersion(t *testing.T) {
	cfg, err := loadConfig([]string{"-version"}, func(string) string { return "" }, func(string) (string, bool) { return "", false }, io.Discard, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Version {
		t.Fatalf("expected Version to be true")
	}
}

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
		func(k string) (string, bool) { v, ok := env[k]; return v, ok }, io.Discard, false)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Embed.Provider != "local" {
		t.Fatalf("provider: %q", cfg.Embed.Provider)
	}

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

func TestRunRoutesMCPSubcommand(t *testing.T) {
	oldArgs, oldStdin, oldStdout := os.Args, os.Stdin, os.Stdout
	defer func() { os.Args, os.Stdin, os.Stdout = oldArgs, oldStdin, oldStdout }()

	dir := t.TempDir()
	os.Args = []string{"dolmen", "mcp", "-data", dir}

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	os.Stdin = inR
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_namespace","arguments":{"namespace":"rt"}}}` + "\n"
	if _, err := inW.WriteString(line); err != nil {
		t.Fatalf("seed stdin: %v", err)
	}
	inW.Close()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	os.Stdout = outW

	oldProvider, hadProvider := os.LookupEnv("DOLMEN_EMBED_PROVIDER")
	os.Setenv("DOLMEN_EMBED_PROVIDER", "none")
	t.Cleanup(func() {
		if hadProvider {
			os.Setenv("DOLMEN_EMBED_PROVIDER", oldProvider)
		} else {
			os.Unsetenv("DOLMEN_EMBED_PROVIDER")
		}
	})

	runErr := make(chan error, 1)
	go func() { runErr <- run() }()
	if err := <-runErr; err != nil {
		t.Fatalf("run dolmen mcp: %v", err)
	}
	outW.Close()
	out, _ := io.ReadAll(outR)

	var res map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		t.Fatalf("stdout must carry one JSON-RPC line, got %q", out)
	}
	if res["id"] != float64(1) || res["result"] == nil {
		t.Fatalf("unexpected response: %v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "rt.db")); err != nil {
		t.Fatalf("the stdio subcommand must operate the store: %v", err)
	}
}

func TestLoadConfigStdioSkipsSubscriptionAgeValidation(t *testing.T) {
	env := map[string]string{
		"DOLMEN_EMBED_PROVIDER":       "none",
		"DOLMEN_MAX_SUBSCRIPTION_AGE": "500ms",
	}
	cfg, err := loadConfig([]string{}, func(k string) string { return env[k] },
		func(k string) (string, bool) { v, ok := env[k]; return v, ok }, io.Discard, true)
	if err != nil {
		t.Fatalf("the stdio mode must not reject an HTTP-only max subscription age it never uses: %v", err)
	}
	if cfg.MaxSubscriptionAge != 0 {
		t.Fatalf("the stdio mode leaves the unused bound at zero, got %v", cfg.MaxSubscriptionAge)
	}
	_, err = loadConfig([]string{}, func(k string) string { return env[k] },
		func(k string) (string, bool) { v, ok := env[k]; return v, ok }, io.Discard, false)
	if err == nil {
		t.Fatal("the serve mode must still reject an out-of-range max subscription age")
	}
}

func loadWithEnv(t *testing.T, args []string, env map[string]string, stdio bool) (*config, error) {
	t.Helper()
	getenv := func(k string) string { return env[k] }
	lookup := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
	return loadConfig(args, getenv, lookup, io.Discard, stdio)
}

func TestLoadConfigAuthMode(t *testing.T) {
	const key = "Tt5vQ2rXm9LbHc0wPqZaJ4yNfE7sUgKdRi1oCnBxV3M"

	cfg, err := loadWithEnv(t, nil, map[string]string{"DOLMEN_AUTH": "on", "DOLMEN_ADMIN_KEY": key, "DOLMEN_EMBED_PROVIDER": "none"}, false)
	if err != nil {
		t.Fatalf("auth on with an admin key: %v", err)
	}
	if !cfg.Auth.On() {
		t.Fatal("DOLMEN_AUTH=on did not turn auth on")
	}

	cfg, err = loadWithEnv(t, []string{"-auth", "on"}, map[string]string{"DOLMEN_ADMIN_KEY": key, "DOLMEN_EMBED_PROVIDER": "none"}, false)
	if err != nil {
		t.Fatalf("-auth on with an admin key: %v", err)
	}
	if !cfg.Auth.On() {
		t.Fatal("-auth on did not turn auth on")
	}

	for name, tc := range map[string]struct {
		env     map[string]string
		args    []string
		stdio   bool
		wantErr string
	}{
		"malformed admin key": {
			env:     map[string]string{"DOLMEN_AUTH": "on", "DOLMEN_ADMIN_KEY": "short"},
			wantErr: "DOLMEN_ADMIN_KEY",
		},
		"admin key with the reserved key prefix": {
			env:     map[string]string{"DOLMEN_AUTH": "on", "DOLMEN_ADMIN_KEY": "dlm_" + key},
			wantErr: "dlm_",
		},
		"malformed admin key rejected even with auth off": {
			env:     map[string]string{"DOLMEN_ADMIN_KEY": "short"},
			wantErr: "DOLMEN_ADMIN_KEY",
		},
		"unknown mode": {
			env:     map[string]string{"DOLMEN_AUTH": "true"},
			wantErr: "invalid auth mode",
		},
		"on over stdio": {
			env:     map[string]string{"DOLMEN_AUTH": "on", "DOLMEN_ADMIN_KEY": key},
			stdio:   true,
			wantErr: "stdio",
		},
	} {
		t.Run(name, func(t *testing.T) {
			tc.env["DOLMEN_EMBED_PROVIDER"] = "none"
			_, err := loadWithEnv(t, tc.args, tc.env, tc.stdio)
			if err == nil {
				t.Fatalf("config accepted, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadConfigAuthOffIsDefault(t *testing.T) {
	cfg, err := loadWithEnv(t, nil, map[string]string{"DOLMEN_EMBED_PROVIDER": "none"}, false)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.Auth.On() {
		t.Fatal("auth defaults to on")
	}
}

func TestLoadConfigStdioAuthOffStillWorks(t *testing.T) {
	cfg, err := loadWithEnv(t, nil, map[string]string{"DOLMEN_EMBED_PROVIDER": "none"}, true)
	if err != nil {
		t.Fatalf("stdio with auth off: %v", err)
	}
	if cfg.Auth.On() {
		t.Fatal("stdio auth is on")
	}
}

func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestLogAuthPosture(t *testing.T) {
	const key = "Tt5vQ2rXm9LbHc0wPqZaJ4yNfE7sUgKdRi1oCnBxV3M"

	off, err := auth.New(auth.Config{Mode: auth.ModeOff})
	if err != nil {
		t.Fatalf("auth off: %v", err)
	}
	got := captureLogs(t, func() { logAuthPosture(off) })
	if !strings.Contains(got, "no authentication") {
		t.Fatalf("auth off did not warn: %q", got)
	}

	on, err := auth.New(auth.Config{Mode: auth.ModeOn, AdminKey: key})
	if err != nil {
		t.Fatalf("auth on: %v", err)
	}
	got = captureLogs(t, func() { logAuthPosture(on) })
	if strings.Contains(got, "no authentication") {
		t.Fatalf("auth on claimed there is no authentication: %q", got)
	}
	if !strings.Contains(got, auth.AdminKeySourceName) {
		t.Fatalf("auth on did not name the enabled source: %q", got)
	}
	if strings.Contains(got, key) {
		t.Fatalf("startup log echoes the admin key: %q", got)
	}
}

func TestLoadConfigMaxGroupsRange(t *testing.T) {
	for _, raw := range []string{"1", "128", "1024"} {
		if _, err := loadWithEnv(t, nil, map[string]string{"DOLMEN_MAX_GROUPS": raw, "DOLMEN_EMBED_PROVIDER": "none"}, false); err != nil {
			t.Fatalf("DOLMEN_MAX_GROUPS=%s rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{"0", "-1", "1025", "many"} {
		if _, err := loadWithEnv(t, nil, map[string]string{"DOLMEN_MAX_GROUPS": raw, "DOLMEN_EMBED_PROVIDER": "none"}, false); err == nil {
			t.Fatalf("DOLMEN_MAX_GROUPS=%s accepted, but the documented range is 1 to 1024", raw)
		}
	}
	for _, raw := range []string{"0", "1025"} {
		if _, err := loadWithEnv(t, []string{"-max-groups", raw}, map[string]string{"DOLMEN_EMBED_PROVIDER": "none"}, false); err == nil {
			t.Fatalf("-max-groups %s accepted, but the documented range is 1 to 1024", raw)
		}
	}
}

func TestAuthOnWithoutASourceFailsWhenTheRegistryOpens(t *testing.T) {
	cfg, err := loadWithEnv(t, nil, map[string]string{
		"DOLMEN_AUTH":           "on",
		"DOLMEN_EMBED_PROVIDER": "none",
	}, false)
	if err != nil {
		t.Fatalf("the source check now runs where API keys are visible, so config should parse: %v", err)
	}
	cfg.DataDir = t.TempDir()

	r, err := openGrantRegistry(cfg)
	if err == nil {
		r.Close()
		t.Fatal("auth on with no identity source started")
	}
	if !strings.Contains(err.Error(), "DOLMEN_ADMIN_KEY") {
		t.Fatalf("error does not name the remediation: %v", err)
	}
}

func TestAuthOnWithAnAdminKeyOpensTheRegistry(t *testing.T) {
	cfg, err := loadWithEnv(t, nil, map[string]string{
		"DOLMEN_AUTH":           "on",
		"DOLMEN_ADMIN_KEY":      "Tt5vQ2rXm9LbHc0wPqZaJ4yNfE7sUgKdRi1oCnBxV3M",
		"DOLMEN_EMBED_PROVIDER": "none",
	}, false)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.DataDir = t.TempDir()
	r, err := openGrantRegistry(cfg)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	defer r.Close()
}
