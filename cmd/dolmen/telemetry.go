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
	stderr := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(telemetry.LogHandler(stderr)))
	p, err := telemetry.Setup(context.Background(), getenv, version.Version)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(p.LogHandler(stderr, level)))
	return p, nil
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
			slog.Warn("telemetry: could not flush every span, metric or log before exit; the collector may be unreachable (OTEL_EXPORTER_OTLP_ENDPOINT)", "err", err)
		}
		return nil
	}
}
