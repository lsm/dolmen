package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/mcp"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/version"
	"github.com/lsm/dolmen/skill"
)

// envHelp documents environment variables that are not represented by flags.
// They are printed after the flag help so a fresh operator can discover the
// embedding provider without reading the skill docs.
type envHelp struct {
	key  string
	desc string
}

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var pe *printedError
		if errors.As(err, &pe) {
			os.Exit(1)
		}
		slog.Error("dolmen exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Args[1:], os.Getenv, os.LookupEnv, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if cfg.Version {
		fmt.Println("dolmen", version.Version)
		return nil
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	st, err := store.Open(cfg.DataDir, store.WithChangeRetention(cfg.ChangeRetention))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	emb, err := embed.NewProvider(cfg.Embed.Provider, cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("embed provider: %w", err)
	}
	if l, ok := emb.(*embed.Local); ok {
		slog.Info("local embedding provider", "model", l.Model, "cache", "under the data directory (first use loads from the cache; downloads from the Hugging Face Hub only if the model is not pre-seeded)")
		if !l.Cached() {
			slog.Warn("local embedding model is not cached; the first vectorized write will download it from the Hugging Face Hub", "model", l.Model)
		}
	}

	apiSrv := api.New(st, emb, api.WithBaseURL(cfg.BaseURL), api.WithNamespaceHint(cfg.SkillNamespaceHint), api.WithPrefix(cfg.Prefix))
	mcpSrv := mcp.New(apiSrv, cfg.AllowedOrigins, mcp.WithBaseURL(cfg.BaseURL), mcp.WithNamespaceHint(cfg.SkillNamespaceHint), mcp.WithPrefix(cfg.Prefix))

	sub := http.NewServeMux()
	sub.Handle("/mcp", mcpSrv)
	sub.Handle("/", apiSrv.Handler())
	router := withPrefix(cfg.Prefix, sub)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.OriginGuard(router, cfg.AllowedOrigins),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("dolmen listening", "addr", cfg.Addr, "data", cfg.DataDir, "embed", emb.Name(), "version", version.Version)
		slog.Info("endpoints", "mcp", "http://"+cfg.Addr+cfg.Prefix+"/mcp", "api", "http://"+cfg.Addr+cfg.Prefix+"/v1/{op}", "health", "http://"+cfg.Addr+cfg.Prefix+"/healthz", "version", "http://"+cfg.Addr+cfg.Prefix+"/version", "skills", "http://"+cfg.Addr+cfg.Prefix+"/skills")
		slog.Warn("no authentication: keep this bound to a private interface")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// printedError marks an error that has already been written to the terminal
// together with usage text; main should exit without logging it again.
type printedError struct {
	err error
}

func (e *printedError) Error() string { return e.err.Error() }

type config struct {
	Addr               string
	DataDir            string
	AllowedOrigins     []string
	Embed              embedConfig
	Version            bool
	BaseURL            string
	Prefix             string
	SkillNamespaceHint string
	ChangeRetention    time.Duration
}

type embedConfig struct {
	Provider string
	BaseURL  string
	Model    string
	APIKey   string
}

func loadConfig(args []string, getenv func(string) string, lookupEnv func(string) (string, bool), out io.Writer) (*config, error) {
	fs := flag.NewFlagSet("dolmen", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprint(out, "Usage: dolmen [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	addr := fs.String("addr", envOr("DOLMEN_ADDR", "127.0.0.1:8790", getenv), "listen address")
	dataDir := fs.String("data", envOr("DOLMEN_DATA", "data", getenv), "data directory (one SQLite file per namespace)")
	showVersion := fs.Bool("version", false, "print version and exit")
	publicBaseURL := fs.String("base-url", envOr("DOLMEN_BASE_URL", "", getenv), "public base URL for skills and MCP links (default: use request Host)")
	prefix := fs.String("prefix", envOr("DOLMEN_PREFIX", "", getenv), "mount all endpoints under this URL prefix (pass-through proxy)")
	// A string flag, not fs.Duration: the default comes from the environment
	// either way, and one parse path validates flag and env identically.
	changeRetention := fs.String("change-retention", envOr("DOLMEN_CHANGE_RETENTION", "168h", getenv), "change-log retention: 0 disables pruning (records and cursors never expire); otherwise 1h to 2160h")

	fs.Usage = func() {
		fmt.Fprint(out, "Usage: dolmen [flags]\n\nFlags:\n")
		fs.PrintDefaults()
		printEnvHelp(out)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, &printedError{err}
	}
	if fs.NArg() > 0 {
		e := fmt.Errorf("unexpected positional argument(s): %q", fs.Args())
		fmt.Fprintf(out, "%v\n", e)
		fs.Usage()
		return nil, &printedError{e}
	}
	if *showVersion {
		return &config{Version: true}, nil
	}

	allowedOrigins, err := parseAllowedOrigins(getenv("DOLMEN_ALLOWED_ORIGINS"))
	if err != nil {
		return nil, fmt.Errorf("DOLMEN_ALLOWED_ORIGINS: %w", err)
	}

	provider := envOr("DOLMEN_EMBED_PROVIDER", "local", getenv)
	baseURL := envOr("DOLMEN_EMBED_BASE_URL", "", getenv)
	model := envOr("DOLMEN_EMBED_MODEL", "", getenv)

	apiKey, ok := lookupEnv("DOLMEN_EMBED_API_KEY")
	if !ok {
		apiKey = getenv("OPENAI_API_KEY")
	}

	if _, err := embed.NewProvider(provider, baseURL, model, apiKey, *dataDir); err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}

	retention, err := parseChangeRetention(*changeRetention)
	if err != nil {
		fmt.Fprintf(out, "config: %v\n", err)
		fs.Usage()
		return nil, &printedError{err}
	}

	skillNamespaceHint := envOr("DOLMEN_SKILL_NAMESPACE_HINT", skill.DefaultNamespaceHint, getenv)
	prefixValue := skill.NormalizePrefix(*prefix)

	if *publicBaseURL != "" && prefixValue != "" {
		if strings.HasSuffix(strings.TrimRight(*publicBaseURL, "/"), prefixValue) {
			e := fmt.Errorf("-base-url %q already ends with -prefix %q: remove one of them, or set -base-url to the scheme://host part and let -prefix supply the path", *publicBaseURL, prefixValue)
			fmt.Fprintf(out, "%v\n", e)
			fs.Usage()
			return nil, &printedError{e}
		}
	}

	return &config{
		Addr:               *addr,
		DataDir:            *dataDir,
		AllowedOrigins:     allowedOrigins,
		BaseURL:            *publicBaseURL,
		Prefix:             prefixValue,
		SkillNamespaceHint: skillNamespaceHint,
		ChangeRetention:    retention,
		Embed: embedConfig{
			Provider: provider,
			BaseURL:  baseURL,
			Model:    model,
			APIKey:   apiKey,
		},
		Version: *showVersion,
	}, nil
}

// parseChangeRetention validates the change-log retention knob (§9.2/§9.3:
// -change-retention / DOLMEN_CHANGE_RETENTION): 0 disables pruning — cursors
// never expire and records accumulate — and any other value must fall in
// 1h–2160h. Out-of-range values are startup-rejected, never silently
// clamped: a typo'd "1m" must not quietly discard a week of backlog, and
// "9999h" must not quietly retain forever.
func parseChangeRetention(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid change retention %q: %w", raw, err)
	}
	if d == 0 || (d >= time.Hour && d <= 2160*time.Hour) {
		return d, nil
	}
	return 0, fmt.Errorf("invalid change retention %q: must be 0 (disable pruning) or between 1h and 2160h", raw)
}

func envOr(key, fallback string, getenv func(string) string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

func printEnvHelp(out io.Writer) {
	help := []envHelp{
		{"DOLMEN_ADDR", "listen address (default 127.0.0.1:8790)"},
		{"DOLMEN_DATA", "data directory (default data)"},
		{"DOLMEN_ALLOWED_ORIGINS", "comma-separated allowed HTTP origins for CORS"},
		{"DOLMEN_BASE_URL", "public base URL for skills and MCP links (default: use request Host)"},
		{"DOLMEN_SKILL_NAMESPACE_HINT", "hint text rendered into skill markdown"},
		{"DOLMEN_CHANGE_RETENTION", "change-log retention: 0 disables pruning, else 1h to 2160h (default 168h)"},
		{"", ""},
		{"DOLMEN_EMBED_PROVIDER", "embedding provider: none, local (default), or openai"},
		{"DOLMEN_EMBED_MODEL", "model name or absolute model-directory path"},
		{"DOLMEN_EMBED_BASE_URL", "base URL for an OpenAI-compatible provider"},
		{"DOLMEN_EMBED_API_KEY", "API key for an OpenAI-compatible provider"},
		{"OPENAI_API_KEY", "fallback API key when DOLMEN_EMBED_API_KEY is unset"},
		{"REMBED_CACHE", "model cache directory for the local provider"},
		{"HF_TOKEN", "Hugging Face token for gated repos downloaded by the local provider"},
	}

	fmt.Fprintln(out, "\nEnvironment variables:")
	for _, h := range help {
		if h.key == "" {
			fmt.Fprintln(out)
			continue
		}
		fmt.Fprintf(out, "  %s  %s\n", h.key, h.desc)
	}
}

func parseAllowedOrigins(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out, nil
}

// withPrefix mounts next under the given URL prefix. Requests outside the
// prefix return 404; requests matching the prefix have it stripped before
// being passed to next.
func withPrefix(prefix string, next http.Handler) http.Handler {
	prefix = skill.NormalizePrefix(prefix)
	if prefix == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) || (len(r.URL.Path) > len(prefix) && r.URL.Path[len(prefix)] != '/') {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = r.URL.Path[len(prefix):]
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		if r2.URL.RawPath != "" {
			if strings.HasPrefix(r2.URL.RawPath, prefix) {
				r2.URL.RawPath = r2.URL.RawPath[len(prefix):]
				if r2.URL.RawPath == "" {
					r2.URL.RawPath = "/"
				}
			} else {
				r2.URL.RawPath = ""
			}
		}
		next.ServeHTTP(w, r2)
	})
}
