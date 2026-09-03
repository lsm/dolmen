package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
)

func main() {
	if err := run(); err != nil {
		slog.Error("dolmen exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", envOr("DOLMEN_ADDR", "127.0.0.1:8790"), "listen address")
	dataDir := flag.String("data", envOr("DOLMEN_DATA", "data"), "data directory (one SQLite file per namespace)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("dolmen", version.Version)
		return nil
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	st, err := store.Open(*dataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	var allowedOrigins []string
	for _, o := range strings.Split(os.Getenv("DOLMEN_ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowedOrigins = append(allowedOrigins, o)
		}
	}

	emb := embed.FromEnv()
	apiSrv := api.New(st, emb)
	mcpSrv := mcp.New(apiSrv, allowedOrigins)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpSrv)
	mux.Handle("/", apiSrv.Handler())

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           api.OriginGuard(mux, allowedOrigins),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("dolmen listening", "addr", *addr, "data", *dataDir, "embed", emb.Name(), "version", version.Version)
		slog.Info("endpoints", "mcp", "http://"+*addr+"/mcp", "api", "http://"+*addr+"/v1/{op}", "health", "http://"+*addr+"/healthz", "version", "http://"+*addr+"/version")
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
