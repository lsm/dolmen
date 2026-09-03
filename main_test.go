package main

import (
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/version"
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
			env:  map[string]string{},
			want: &config{
				Addr:           "127.0.0.1:8790",
				DataDir:        "data",
				AllowedOrigins: nil,
				Embed:          embedConfig{Provider: "none"},
			},
		},
		{
			name: "flags override env and defaults",
			args: []string{"-addr", ":8080", "-data", "/tmp/data"},
			env:  map[string]string{"DOLMEN_ADDR": ":9999", "DOLMEN_DATA": "/wrong"},
			want: &config{
				Addr:           ":8080",
				DataDir:        "/tmp/data",
				AllowedOrigins: nil,
				Embed:          embedConfig{Provider: "none"},
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
			},
		},
		{
			name:    "unknown embed provider is rejected",
			args:    []string{},
			env:     map[string]string{"DOLMEN_EMBED_PROVIDER": "foo"},
			wantErr: "unknown embedding provider",
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
