package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/lsm/dolmen/internal/telemetry"
	"github.com/lsm/dolmen/internal/version"
)

const telemetryFlushBound = 10 * time.Second

func startTelemetry(level slog.Level, getenv func(string) string) (*telemetry.Provider, error) {
	slog.SetDefault(slog.New(telemetry.LogHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))))
	return telemetry.Setup(context.Background(), getenv, version.Version)
}

func stopTelemetry(p *telemetry.Provider, grace time.Duration) func() error {
	return func() error {
		bound := telemetryFlushBound
		if grace > 0 && grace < bound {
			bound = grace
		}
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		defer cancel()
		if err := p.Shutdown(ctx); err != nil {
			slog.Warn("telemetry: could not flush every span before exit; the collector may be unreachable (OTEL_EXPORTER_OTLP_ENDPOINT)", "err", err)
		}
		return nil
	}
}
