package main

import (
	"fmt"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"os"
	"path/filepath"
	"runtime"

	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/store"
)

func besideExecutable(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), name)
}

func lakehouseSQLEngine(cfg *config) lakehouse.SQLEngine {
	bin := cfg.DuckDBSidecar
	if bin == "" {
		name := "dolmen-duckdb"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		bin = besideExecutable(name)
	}
	ext := cfg.DuckDBExtensions
	if ext == "" {
		ext = besideExecutable("duckdb-extensions")
	}
	return lakehouse.SQLEngine{Binary: bin, ExtensionDir: ext}
}

func openLakehouse(cfg *config, tp trace.TracerProvider, mp metric.MeterProvider) (store.Engine, error) {
	st, err := lakehouse.Open(cfg.DataDir,
		lakehouse.WithSecretKeyring(cfg.Secrets),
		lakehouse.WithChangeRetention(cfg.ChangeRetention),
		lakehouse.WithMaxOpenNamespaces(cfg.MaxOpenNamespaces),
		lakehouse.WithSQLEngine(lakehouseSQLEngine(cfg)),
		lakehouse.WithSharedFilter(cfg.Auth.On()),
		lakehouse.WithTracerProvider(tp),
		lakehouse.WithMeterProvider(mp))
	if err != nil {
		return nil, fmt.Errorf("open lakehouse store: %w", err)
	}
	return st, nil
}
