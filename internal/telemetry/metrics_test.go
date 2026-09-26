package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/telemetry"
)

func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func metricsOnly(t *testing.T) (*telemetry.Tracing, *sdkmetric.ManualReader) {
	t.Helper()
	r := sdkmetric.NewManualReader()
	tr, err := telemetry.NewWithMeter(nil, sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.On() {
		t.Fatal("metrics alone must switch instrumentation on")
	}
	return tr, r
}

func attrMap(set attribute.Set) map[string]string {
	out := map[string]string{}
	for _, kv := range set.ToSlice() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func TestHTTPServerDurationFollowsSemconvWithBoundedAttributes(t *testing.T) {
	tr, r := metricsOnly(t)
	h := tr.Server("/v1/{op}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/insert?namespace=secretns", nil)
	req.Header.Set("User-Agent", "agent/secret")
	h.ServeHTTP(httptest.NewRecorder(), req)

	m, ok := collect(t, r)[telemetry.MetricHTTPServerDuration]
	if !ok {
		t.Fatalf("no %s recorded", telemetry.MetricHTTPServerDuration)
	}
	if m.Unit != "s" {
		t.Fatalf("unit = %q, want s", m.Unit)
	}
	hist := m.Data.(metricdata.Histogram[float64])
	if len(hist.DataPoints) != 1 || hist.DataPoints[0].Count != 1 {
		t.Fatalf("want one request recorded, got %+v", hist.DataPoints)
	}
	got := attrMap(hist.DataPoints[0].Attributes)
	want := map[string]string{"http.request.method": "POST", "http.route": "/v1/{op}", "http.response.status_code": "500", "url.scheme": "http", "error.type": "500"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for k, v := range got {
		if strings.Contains(v, "secret") || k == "url.path" || k == "client.address" || k == "user_agent.original" {
			t.Errorf("a metric attribute must stay low-cardinality and carry no request data: %s=%s", k, v)
		}
	}
}

func TestOperationMetricsRecordDurationAndInFlight(t *testing.T) {
	tr, r := metricsOnly(t)
	_, op := tr.StartOp(context.Background(), "insert", "req-1", "alice")
	if v := sumOf(t, collect(t, r)[telemetry.MetricOpsInFlight]); v != 1 {
		t.Fatalf("in flight during the operation = %d, want 1", v)
	}
	op.End("conflict")
	got := collect(t, r)
	if v := sumOf(t, got[telemetry.MetricOpsInFlight]); v != 0 {
		t.Fatalf("in flight after the operation = %d, want 0", v)
	}
	hist := got[telemetry.MetricOpDuration].Data.(metricdata.Histogram[float64])
	if len(hist.DataPoints) != 1 {
		t.Fatalf("want one operation series, got %d", len(hist.DataPoints))
	}
	attrs := attrMap(hist.DataPoints[0].Attributes)
	if attrs["dolmen.op.name"] != "insert" || attrs["dolmen.op.outcome"] != "conflict" || len(attrs) != 2 {
		t.Fatalf("operation metric attributes = %v, want exactly op name and outcome", attrs)
	}
}

func sumOf(t *testing.T, m metricdata.Metrics) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

func TestSubscriptionsGaugeRisesAndFalls(t *testing.T) {
	tr, r := metricsOnly(t)
	ctx := context.Background()
	tr.SubscriptionOpened(ctx)
	tr.SubscriptionOpened(ctx)
	tr.SubscriptionClosed(ctx)
	if v := sumOf(t, collect(t, r)[telemetry.MetricSubscriptionsActive]); v != 1 {
		t.Fatalf("active subscriptions = %d, want 1", v)
	}
}

func TestEmbeddingMetricsRecordDurationAndTokensWithoutATracer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`))
	}))
	defer upstream.Close()
	tr, r := metricsOnly(t)
	p := tr.Embedder(&embed.OpenAI{BaseURL: upstream.URL, Model: "m"})
	if _, err := p.Embed(context.Background(), []string{"private words"}); err != nil {
		t.Fatal(err)
	}
	got := collect(t, r)
	dur := got[telemetry.MetricGenAIDuration].Data.(metricdata.Histogram[float64])
	if len(dur.DataPoints) != 1 {
		t.Fatalf("want one embedding call recorded, got %d", len(dur.DataPoints))
	}
	attrs := attrMap(dur.DataPoints[0].Attributes)
	if attrs["gen_ai.operation.name"] != "embeddings" || attrs["gen_ai.request.model"] != "m" || attrs["gen_ai.provider.name"] != "openai" {
		t.Fatalf("embedding duration attributes = %v", attrs)
	}
	tokens := got[telemetry.MetricGenAITokenUsage].Data.(metricdata.Histogram[int64])
	if len(tokens.DataPoints) != 1 || tokens.DataPoints[0].Sum != 7 || attrMap(tokens.DataPoints[0].Attributes)["gen_ai.token.type"] != "input" {
		t.Fatalf("token usage must count the provider's reported input tokens even with tracing off: %+v", tokens.DataPoints)
	}
}

func TestMetricsOffLeavesInstrumentationOff(t *testing.T) {
	tr, err := telemetry.NewWithMeter(nil, nil, nil, false)
	if err != nil || tr.On() {
		t.Fatalf("no tracer and no meter must leave instrumentation off: %v %v", tr.On(), err)
	}
}
