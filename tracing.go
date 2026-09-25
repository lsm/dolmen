package dolmen

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/telemetry"
)

func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		c.tracerProvider = tp
		c.tracerProviderSet = true
	}
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
