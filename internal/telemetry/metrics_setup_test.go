package telemetry

import (
	"context"
	"strings"
	"sync"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type recordedMetrics struct {
	mu      sync.Mutex
	exports []metricdata.ResourceMetrics
}

func (r *recordedMetrics) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}

func (r *recordedMetrics) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

func (r *recordedMetrics) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exports = append(r.exports, *rm)
	return nil
}

func (r *recordedMetrics) ForceFlush(context.Context) error { return nil }

func (r *recordedMetrics) Shutdown(context.Context) error { return nil }

func (r *recordedMetrics) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rm := range r.exports {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				out = append(out, m.Name)
			}
		}
	}
	return out
}

func metricFactory(called *bool, exp sdkmetric.Exporter) metricExporterFactory {
	return func(context.Context) (sdkmetric.Exporter, error) {
		*called = true
		return exp, nil
	}
}

func TestMetricsAreSwitchedOnIndependentlyOfTraces(t *testing.T) {
	for _, tc := range []struct {
		name            string
		env             map[string]string
		traces, metrics bool
	}{
		{"nothing set", map[string]string{}, false, false},
		{"shared endpoint turns both on", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"}, true, true},
		{"metrics endpoint alone", map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://c:4318/v1/metrics"}, false, true},
		{"traces endpoint alone", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://c:4318/v1/traces"}, true, false},
		{"metrics opted out", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318", "OTEL_METRICS_EXPORTER": "none"}, true, false},
		{"traces opted out", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318", "OTEL_TRACES_EXPORTER": "none"}, false, true},
		{"explicit otlp", map[string]string{"OTEL_METRICS_EXPORTER": "otlp"}, false, true},
		{"sdk disabled", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318", "OTEL_SDK_DISABLED": "true"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tracesCalled, metricsCalled bool
			p, err := setup(context.Background(), envOf(tc.env), "v1",
				recordingFactory(&tracesCalled, tracetest.NewInMemoryExporter()),
				withMetricExporter(metricFactory(&metricsCalled, &recordedMetrics{})))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Shutdown(context.Background())
			if tracesCalled != tc.traces || metricsCalled != tc.metrics {
				t.Fatalf("traces exporter built %v, metrics exporter built %v; want %v, %v", tracesCalled, metricsCalled, tc.traces, tc.metrics)
			}
			if (tc.traces || tc.metrics) != p.Tracing.On() {
				t.Fatalf("instrumentation on = %v, want %v", p.Tracing.On(), tc.traces || tc.metrics)
			}
		})
	}
}

func TestUnsupportedMetricSettingsAreRefusedWithTheFix(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"OTEL_METRICS_EXPORTER": "prometheus"}, "GET /metrics"},
		{map[string]string{"OTEL_METRICS_EXPORTER": "console"}, "otlp"},
		{map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"}, "http/protobuf"},
		{map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "x", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, "http/protobuf"},
	} {
		var called bool
		_, err := setup(context.Background(), envOf(tc.env), "v1",
			recordingFactory(&called, tracetest.NewInMemoryExporter()),
			withMetricExporter(metricFactory(&called, &recordedMetrics{})))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("env %v: want an error mentioning %q, got %v", tc.env, tc.want, err)
		}
	}
}

func TestShutdownFlushesPendingMetrics(t *testing.T) {
	var called bool
	rec := &recordedMetrics{}
	p, err := setup(context.Background(), envOf(map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "x"}), "v1",
		recordingFactory(new(bool), tracetest.NewInMemoryExporter()),
		withMetricExporter(metricFactory(&called, rec)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, op := p.Tracing.StartOp(context.Background(), "insert", "", "")
	_ = ctx
	op.End("ok")
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rec.names(), " "), MetricOpDuration) {
		t.Fatalf("shutdown must flush recorded metrics; exported %v", rec.names())
	}
}

func TestTheTracerProviderReachesTheEngineOnlyWhenTracesAreOn(t *testing.T) {
	for _, tc := range []struct {
		env    map[string]string
		traces bool
	}{
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"}, true},
		{map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://c:4318/v1/metrics"}, false},
	} {
		p, err := setup(context.Background(), envOf(tc.env), "v1",
			recordingFactory(new(bool), tracetest.NewInMemoryExporter()),
			withMetricExporter(metricFactory(new(bool), &recordedMetrics{})))
		if err != nil {
			t.Fatal(err)
		}
		if (p.TracerProvider != nil) != tc.traces {
			t.Errorf("env %v: TracerProvider set = %v, want %v (storage spans reach the engine through it)", tc.env, p.TracerProvider != nil, tc.traces)
		}
		p.Shutdown(context.Background())
	}
}
