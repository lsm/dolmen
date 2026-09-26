package telemetry

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/ops"
)

const (
	GenAIOperationNameKey    = attribute.Key("gen_ai.operation.name")
	GenAIRequestModelKey     = attribute.Key("gen_ai.request.model")
	GenAIProviderNameKey     = attribute.Key("gen_ai.provider.name")
	GenAIUsageInputTokensKey = attribute.Key("gen_ai.usage.input_tokens")
	EmbedBatchSizeKey        = attribute.Key("dolmen.embed.batch_size")
)

type EmbeddingProvider interface {
	Identity() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

type tracedEmbedder struct {
	inner EmbeddingProvider
	t     *Tracing
	kind  trace.SpanKind
	attrs []attribute.KeyValue
	model string
}

func (t *Tracing) Embedder(p EmbeddingProvider) EmbeddingProvider {
	if !t.On() || p == nil {
		return p
	}
	te := &tracedEmbedder{inner: p, t: t, kind: trace.SpanKindInternal}
	te.attrs = append(te.attrs, GenAIOperationNameKey.String("embeddings"))
	if n, ok := p.(interface{ ModelName() string }); ok {
		te.model = n.ModelName()
	}
	if te.model != "" {
		te.attrs = append(te.attrs, GenAIRequestModelKey.String(te.model))
	}
	switch v := p.(type) {
	case *embed.OpenAI:
		te.kind = trace.SpanKindClient
		te.attrs = append(te.attrs, GenAIProviderNameKey.String("openai"))
		if u, err := url.Parse(strings.TrimRight(v.BaseURL, "/")); err == nil && u.Hostname() != "" {
			te.attrs = append(te.attrs, semconv.ServerAddress(u.Hostname()))
			if port, err := strconv.Atoi(u.Port()); err == nil {
				te.attrs = append(te.attrs, semconv.ServerPort(port))
			}
		}
	case *embed.Local:
		te.attrs = append(te.attrs, GenAIProviderNameKey.String("dolmen.local"))
	default:
		if n, ok := p.(interface{ Name() string }); ok && n.Name() != "" {
			te.attrs = append(te.attrs, GenAIProviderNameKey.String(n.Name()))
		}
	}
	return te
}

func (e *tracedEmbedder) Name() string {
	if n, ok := e.inner.(interface{ Name() string }); ok {
		return n.Name()
	}
	return ""
}

func (e *tracedEmbedder) ModelName() string { return e.model }

func (e *tracedEmbedder) Identity() string { return e.inner.Identity() }

type embedCall struct {
	ctx    context.Context
	span   trace.Span
	start  time.Time
	tokens atomic.Int64
}

func (e *tracedEmbedder) start(ctx context.Context, n int) *embedCall {
	name := "embeddings"
	if e.model != "" {
		name += " " + e.model
	}
	attrs := append(append([]attribute.KeyValue(nil), e.attrs...), EmbedBatchSizeKey.Int(n))
	ctx, span := e.t.tracer.Start(ctx, name, trace.WithSpanKind(e.kind), trace.WithAttributes(attrs...))
	c := &embedCall{span: span, start: time.Now()}
	c.ctx = embed.WithUsage(embed.WithTraceContext(ctx), &c.tokens)
	return c
}

func (e *tracedEmbedder) finish(c *embedCall, err error) {
	finishEmbed(c.span, err)
	if e.t.inst == nil {
		return
	}
	attrs := e.attrs
	if err != nil {
		attrs = append(append(make([]attribute.KeyValue, 0, len(e.attrs)+1), e.attrs...), semconv.ErrorTypeKey.String(embedErrorType(err)))
	}
	e.t.inst.embedded(c.ctx, attrs, c.tokens.Load(), time.Since(c.start))
}

func embedErrorType(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return string(derr.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return string(derr.Timeout)
	}
	if code := ops.Classify(err); code != derr.Internal {
		return string(code)
	}
	return string(derr.EmbedderUnavailable)
}

func finishEmbed(span trace.Span, err error) {
	if err != nil {
		span.SetAttributes(semconv.ErrorTypeKey.String(embedErrorType(err)))
		span.SetStatus(codes.Error, "embedding call failed")
	}
	span.End()
}

func (e *tracedEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c := e.start(ctx, len(texts))
	out, err := e.inner.Embed(c.ctx, texts)
	e.finish(c, err)
	return out, err
}

func (e *tracedEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	c := e.start(ctx, 1)
	out, err := e.inner.EmbedQuery(c.ctx, text)
	e.finish(c, err)
	return out, err
}
