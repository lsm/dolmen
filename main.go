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

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	emb, err := embed.NewProvider(cfg.Embed.Provider, cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("embed provider: %w", err)
	}
	if l, ok := emb.(*embed.Local); ok {
		slog.Info("local embedding provider", "model", l.Model, "cache", "under the data directory (first use downloads it from the Hugging Face Hub)")
	}

	apiSrv := api.New(st, emb, api.WithBaseURL(cfg.BaseURL), api.WithNamespaceHint(cfg.SkillNamespaceHint))
	mcpSrv := mcp.New(apiSrv, cfg.AllowedOrigins, mcp.WithBaseURL(cfg.BaseURL), mcp.WithNamespaceHint(cfg.SkillNamespaceHint))

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpSrv)
	mux.Handle("/", apiSrv.Handler())

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.OriginGuard(mux, cfg.AllowedOrigins),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("dolmen listening", "addr", cfg.Addr, "data", cfg.DataDir, "embed", emb.Name(), "version", version.Version)
		slog.Info("endpoints", "mcp", "http://"+cfg.Addr+"/mcp", "api", "http://"+cfg.Addr+"/v1/{op}", "health", "http://"+cfg.Addr+"/healthz", "version", "http://"+cfg.Addr+"/version", "skills", "http://"+cfg.Addr+"/skills")
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
	SkillNamespaceHint string
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

	provider := envOr("DOLMEN_EMBED_PROVIDER", "none", getenv)
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

	skillNamespaceHint := envOr("DOLMEN_SKILL_NAMESPACE_HINT", skill.DefaultNamespaceHint, getenv)

	return &config{
		Addr:               *addr,
		DataDir:            *dataDir,
		AllowedOrigins:     allowedOrigins,
		BaseURL:            *publicBaseURL,
		SkillNamespaceHint: skillNamespaceHint,
		Embed: embedConfig{
			Provider: provider,
			BaseURL:  baseURL,
			Model:    model,
			APIKey:   apiKey,
		},
		Version: *showVersion,
	}, nil
}

func envOr(key, fallback string, getenv func(string) string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
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
