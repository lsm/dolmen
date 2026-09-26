package dbspan

import (
	"context"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const instrumentationName = "github.com/lsm/dolmen"

const MaxNameAttr = 256

const (
	SearchKindKey       = attribute.Key("dolmen.search.kind")
	TxOutcomeKey        = attribute.Key("dolmen.tx.outcome")
	VectorCacheKey      = attribute.Key("dolmen.vector.cache")
	VectorRowsScoredKey = attribute.Key("dolmen.vector.rows_scored")
	VectorCandidatesKey = attribute.Key("dolmen.vector.candidates")
	MigrateStepIndexKey = attribute.Key("dolmen.migrate.step.index")
	MigrateStepKindKey  = attribute.Key("dolmen.migrate.step.kind")
)

const (
	WriterWait  = "dolmen.writer.wait"
	Transaction = "dolmen.transaction"
	VectorCache = "dolmen.vector.cache"
	VectorScore = "dolmen.vector.score"
	MigrateStep = "dolmen.migrate.step"
)

type Tracer struct {
	t        trace.Tracer
	base     []attribute.KeyValue
	kind     trace.SpanKind
	classify func(error) string
}

func New(tp trace.TracerProvider, classify func(error) string, kind trace.SpanKind, base ...attribute.KeyValue) *Tracer {
	if tp == nil {
		return nil
	}
	if _, off := tp.(noop.TracerProvider); off {
		return nil
	}
	return &Tracer{t: tp.Tracer(instrumentationName), base: base, kind: kind, classify: classify}
}

func (t *Tracer) On() bool { return t != nil }

var noSpan = trace.SpanFromContext(context.Background())

func (t *Tracer) Op(ctx context.Context, op, namespace, table string, extra ...attribute.KeyValue) (context.Context, trace.Span) {
	if t == nil {
		return ctx, noSpan
	}
	name := op
	attrs := make([]attribute.KeyValue, 0, len(t.base)+3+len(extra))
	attrs = append(attrs, t.base...)
	attrs = append(attrs, semconv.DBOperationName(op))
	if namespace != "" {
		attrs = append(attrs, semconv.DBNamespace(Clean(namespace, MaxNameAttr)))
	}
	if table != "" {
		table = Clean(table, MaxNameAttr)
		attrs = append(attrs, semconv.DBCollectionName(table))
		name += " " + table
	}
	attrs = append(attrs, extra...)
	return t.t.Start(ctx, name, trace.WithSpanKind(t.kind), trace.WithAttributes(attrs...))
}

func (t *Tracer) Child(ctx context.Context, name string, extra ...attribute.KeyValue) (context.Context, trace.Span) {
	if t == nil {
		return ctx, noSpan
	}
	return t.t.Start(ctx, name, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(extra...))
}

func (t *Tracer) End(span trace.Span, err error) {
	if t == nil {
		return
	}
	if err != nil {
		code := "internal_error"
		if t.classify != nil {
			code = t.classify(err)
		}
		span.SetAttributes(semconv.ErrorTypeKey.String(code))
		span.SetStatus(codes.Error, code)
	}
	span.End()
}

func Clean(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

type providerKey struct{}

func ContextWithProvider(ctx context.Context, tp trace.TracerProvider) context.Context {
	if tp == nil {
		return ctx
	}
	return context.WithValue(ctx, providerKey{}, tp)
}

func ProviderFrom(ctx context.Context) trace.TracerProvider {
	tp, _ := ctx.Value(providerKey{}).(trace.TracerProvider)
	return tp
}
