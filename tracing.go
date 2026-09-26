package dolmen

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/telemetry"
)

func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		c.tracerProvider = tp
		c.tracerProviderSet = true
	}
}

func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) {
		c.meterProvider = mp
		c.meterProviderSet = true
	}
}

func (c *config) instrumentation() (*telemetry.Tracing, error) {
	t, err := telemetry.NewWithMeter(c.tracerProvider, c.meterProvider, nil, false)
	if err != nil {
		return nil, derr.Wrap(derr.InvalidRequest, fmt.Errorf("WithMeterProvider: %w", err))
	}
	return t, nil
}

func (s *Store) startOp(ctx context.Context, op, namespace, table string) (context.Context, *telemetry.OpSpan) {
	if !s.tracing.On() {
		return ctx, nil
	}
	ctx, span := s.tracing.StartOp(ctx, op, "", "")
	span.SetScope(ops.NormalizeNamespace(namespace), ops.NormalizeTable(table))
	return ctx, span
}

func endOp(span *telemetry.OpSpan, err error) {
	if span == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = string(ops.Classify(err))
	}
	span.End(outcome)
}
