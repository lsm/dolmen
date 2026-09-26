package telemetry

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"
)

type logHandler struct {
	next slog.Handler
}

func LogHandler(next slog.Handler) slog.Handler {
	return logHandler{next: next}
}

func (h logHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			r = r.Clone()
			r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
		}
	}
	return h.next.Handle(ctx, r)
}

func (h logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logHandler{next: h.next.WithAttrs(attrs)}
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return logHandler{next: h.next.WithGroup(name)}
}

func (p *Provider) LogHandler(next slog.Handler, level slog.Leveler) slog.Handler {
	local := LogHandler(next)
	if p == nil || p.logs == nil {
		return local
	}
	exported := otelslog.NewHandler(instrumentationName, otelslog.WithLoggerProvider(p.logs))
	return fanout{local, levelGate{Handler: exported, level: level}}
}

type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

type levelGate struct {
	slog.Handler
	level slog.Leveler
}

func (g levelGate) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= g.level.Level() && g.Handler.Enabled(ctx, l)
}

func (g levelGate) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelGate{Handler: g.Handler.WithAttrs(attrs), level: g.level}
}

func (g levelGate) WithGroup(name string) slog.Handler {
	return levelGate{Handler: g.Handler.WithGroup(name), level: g.level}
}
