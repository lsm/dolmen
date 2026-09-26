package telemetry

import (
	"context"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

var (
	httpDurationBuckets  = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}
	opDurationBuckets    = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 30, 120}
	genAIDurationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	genAITokenBuckets    = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
)

const (
	MetricHTTPServerDuration  = "http.server.request.duration"
	MetricOpDuration          = "dolmen.operation.duration"
	MetricOpsInFlight         = "dolmen.operations.in_flight"
	MetricSubscriptionsActive = "dolmen.subscriptions.active"
	MetricGenAIDuration       = "gen_ai.client.operation.duration"
	MetricGenAITokenUsage     = "gen_ai.client.token.usage"
	genAITokenTypeKey         = attribute.Key("gen_ai.token.type")
)

type instruments struct {
	httpDuration  metric.Float64Histogram
	opDuration    metric.Float64Histogram
	opsInFlight   metric.Int64UpDownCounter
	subscriptions metric.Int64UpDownCounter
	genAIDuration metric.Float64Histogram
	genAITokens   metric.Int64Histogram
}

func newInstruments(mp metric.MeterProvider) (*instruments, error) {
	m := mp.Meter(instrumentationName)
	var in instruments
	var err error
	if in.httpDuration, err = m.Float64Histogram(MetricHTTPServerDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of HTTP server requests."),
		metric.WithExplicitBucketBoundaries(httpDurationBuckets...)); err != nil {
		return nil, err
	}
	if in.opDuration, err = m.Float64Histogram(MetricOpDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of dolmen operations by operation and outcome (ok or an error code); its count is the number of operations."),
		metric.WithExplicitBucketBoundaries(opDurationBuckets...)); err != nil {
		return nil, err
	}
	if in.opsInFlight, err = m.Int64UpDownCounter(MetricOpsInFlight,
		metric.WithUnit("{operation}"),
		metric.WithDescription("Operations running now.")); err != nil {
		return nil, err
	}
	if in.subscriptions, err = m.Int64UpDownCounter(MetricSubscriptionsActive,
		metric.WithUnit("{subscription}"),
		metric.WithDescription("Open /v1/subscribe streams.")); err != nil {
		return nil, err
	}
	if in.genAIDuration, err = m.Float64Histogram(MetricGenAIDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of calls to the embedding provider."),
		metric.WithExplicitBucketBoundaries(genAIDurationBuckets...)); err != nil {
		return nil, err
	}
	if in.genAITokens, err = m.Int64Histogram(MetricGenAITokenUsage,
		metric.WithUnit("{token}"),
		metric.WithDescription("Input tokens per embedding call, when the provider reports them."),
		metric.WithExplicitBucketBoundaries(genAITokenBuckets...)); err != nil {
		return nil, err
	}
	return &in, nil
}

func (in *instruments) opStarted(ctx context.Context, op string) {
	if in == nil {
		return
	}
	in.opsInFlight.Add(ctx, 1, metric.WithAttributes(OpNameKey.String(op)))
}

func (in *instruments) opEnded(ctx context.Context, op, outcome string, d time.Duration) {
	if in == nil {
		return
	}
	in.opsInFlight.Add(ctx, -1, metric.WithAttributes(OpNameKey.String(op)))
	in.opDuration.Record(ctx, d.Seconds(), metric.WithAttributes(OpNameKey.String(op), OpOutcomeKey.String(outcome)))
}

func (in *instruments) httpServed(ctx context.Context, attrs []attribute.KeyValue, d time.Duration) {
	if in == nil {
		return
	}
	in.httpDuration.Record(ctx, d.Seconds(), metric.WithAttributes(attrs...))
}

func (in *instruments) embedded(ctx context.Context, attrs []attribute.KeyValue, inputTokens int64, d time.Duration) {
	if in == nil {
		return
	}
	in.genAIDuration.Record(ctx, d.Seconds(), metric.WithAttributes(attrs...))
	if inputTokens > 0 {
		tokenAttrs := append(append(make([]attribute.KeyValue, 0, len(attrs)+1), attrs...), genAITokenTypeKey.String("input"))
		in.genAITokens.Record(ctx, inputTokens, metric.WithAttributes(tokenAttrs...))
	}
}

func (t *Tracing) SubscriptionOpened(ctx context.Context) {
	if t == nil {
		return
	}
	if t.inst != nil {
		t.inst.subscriptions.Add(ctx, 1)
	}
}

func (t *Tracing) SubscriptionClosed(ctx context.Context) {
	if t == nil {
		return
	}
	if t.inst != nil {
		t.inst.subscriptions.Add(ctx, -1)
	}
}

func httpMetricAttrs(method, route, scheme, protocol string, status int) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(method),
		semconv.HTTPRoute(route),
		semconv.URLScheme(scheme),
		semconv.HTTPResponseStatusCode(status),
	}
	if protocol != "" {
		attrs = append(attrs, semconv.NetworkProtocolVersion(protocol))
	}
	if status >= 500 {
		attrs = append(attrs, semconv.ErrorTypeKey.String(strconv.Itoa(status)))
	}
	return attrs
}
