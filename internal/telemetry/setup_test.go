package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

type keptExporter struct {
	*tracetest.InMemoryExporter
}

func (keptExporter) Shutdown(context.Context) error { return nil }

func recordingFactory(called *bool, exp sdktrace.SpanExporter) exporterFactory {
	return func(context.Context) (sdktrace.SpanExporter, error) {
		*called = true
		return exp, nil
	}
}

func TestSetupIsOffWithoutAnEndpoint(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"empty":            {},
		"sdk disabled":     {"OTEL_SDK_DISABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"},
		"exporter none":    {"OTEL_TRACES_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"},
		"only service env": {"OTEL_SERVICE_NAME": "x", "OTEL_TRACES_SAMPLER": "always_on"},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			p, err := setup(context.Background(), envOf(env), "v1", recordingFactory(&called, tracetest.NewInMemoryExporter()))
			if err != nil {
				t.Fatal(err)
			}
			if called || p.Tracing.On() {
				t.Fatalf("tracing must stay off: exporter built=%v on=%v", called, p.Tracing.On())
			}
			if err := p.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSetupOnWithEndpointOrExplicitExporter(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"endpoint":        {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"},
		"traces endpoint": {"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"},
		"explicit otlp":   {"OTEL_TRACES_EXPORTER": "otlp", "OTEL_SDK_DISABLED": "false"},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			p, err := setup(context.Background(), envOf(env), "v1", recordingFactory(&called, tracetest.NewInMemoryExporter()))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Shutdown(context.Background())
			if !called || !p.Tracing.On() {
				t.Fatalf("tracing must be on: exporter built=%v", called)
			}
		})
	}
}

func TestSetupRejectsUnsupportedSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"grpc":       {map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, "http/protobuf"},
		"sampler":    {map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "OTEL_TRACES_SAMPLER": "jaeger_remote"}, "OTEL_TRACES_SAMPLER"},
		"ratio":      {map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "2"}, "between 0 and 1"},
		"propagator": {map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "OTEL_PROPAGATORS": "b3"}, "OTEL_PROPAGATORS"},
		"exporter":   {map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"}, "OTEL_TRACES_EXPORTER"},
		"principal":  {map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "DOLMEN_OTEL_INCLUDE_PRINCIPAL": "yes"}, "DOLMEN_OTEL_INCLUDE_PRINCIPAL"},
		"attrs":      {map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "OTEL_RESOURCE_ATTRIBUTES": "novalue"}, "key=value"},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			_, err := setup(context.Background(), envOf(tc.env), "v1", recordingFactory(&called, tracetest.NewInMemoryExporter()))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func resourceOf(t *testing.T, env map[string]string) map[attribute.Key]string {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	called := false
	p, err := setup(context.Background(), envOf(env), "v9.9.9", recordingFactory(&called, keptExporter{exp}))
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracing.tracer.Start(context.Background(), "probe")
	span.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	out := map[attribute.Key]string{}
	for _, kv := range spans[0].Resource.Attributes() {
		out[kv.Key] = kv.Value.Emit()
	}
	return out
}

func TestResourceAttributes(t *testing.T) {
	res := resourceOf(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=prod,team=data%2Cinfra"})
	if res["service.name"] != "dolmen" || res["service.version"] != "v9.9.9" || res["service.instance.id"] == "" {
		t.Fatalf("resource = %v", res)
	}
	if res["deployment.environment.name"] != "prod" || res["team"] != "data,infra" {
		t.Fatalf("OTEL_RESOURCE_ATTRIBUTES not applied: %v", res)
	}
	res = resourceOf(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "OTEL_SERVICE_NAME": "dolmen-eu", "OTEL_RESOURCE_ATTRIBUTES": "service.name=ignored"})
	if res["service.name"] != "dolmen-eu" {
		t.Fatalf("OTEL_SERVICE_NAME must win: %v", res)
	}
	res = resourceOf(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs"})
	if res["service.name"] != "from-attrs" {
		t.Fatalf("OTEL_RESOURCE_ATTRIBUTES service.name must apply without OTEL_SERVICE_NAME: %v", res)
	}
}

func TestDefaultSamplerFollowsParent(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	called := false
	p, err := setup(context.Background(), envOf(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "x"}), "v1", recordingFactory(&called, keptExporter{exp}))
	if err != nil {
		t.Fatal(err)
	}
	h := p.Tracing.Server("/v1/{op}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodPost, "/v1/query", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00")
	h.ServeHTTP(httptest.NewRecorder(), req)
	req = httptest.NewRequest(http.MethodPost, "/v1/query", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	_ = p.Shutdown(context.Background())
	if n := len(exp.GetSpans()); n != 1 {
		t.Fatalf("an unsampled parent must not be exported by default, and a root must be: got %d spans, want 1", n)
	}
}

func newRecorded() (*Tracing, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return New(tp, propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}), false), rec
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

func TestServerSpanFollowsHTTPSemconv(t *testing.T) {
	tr, rec := newRecorded()
	h := tr.Server("/v1/{op}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(http.StatusTeapot)
	}))
	req := httptest.NewRequest(http.MethodPost, "http://db.example:8790/v1/insert", nil)
	req.Header.Set("User-Agent", "probe/1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans", len(spans))
	}
	s := spans[0]
	if s.Name() != "POST /v1/{op}" || s.SpanKind() != trace.SpanKindServer {
		t.Fatalf("name=%q kind=%v", s.Name(), s.SpanKind())
	}
	a := attrs(s)
	want := map[attribute.Key]any{
		"http.request.method":       "POST",
		"http.route":                "/v1/{op}",
		"url.scheme":                "http",
		"url.path":                  "/v1/insert",
		"server.address":            "db.example",
		"server.port":               int64(8790),
		"http.response.status_code": int64(418),
		"user_agent.original":       "probe/1",
		"dolmen.request_id":         "req-1",
	}
	for k, v := range want {
		if a[k].AsInterface() != v {
			t.Errorf("%s = %v, want %v", k, a[k].AsInterface(), v)
		}
	}
	if s.Status().Code == codes.Error {
		t.Fatal("a 4xx must not mark a server span as an error")
	}
}

func TestServerSpanMarksServerErrors(t *testing.T) {
	tr, rec := newRecorded()
	h := tr.Server("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	s := rec.Ended()[0]
	if s.Status().Code != codes.Error || attrs(s)["error.type"].AsString() != "500" {
		t.Fatalf("status=%v attrs=%v", s.Status(), attrs(s))
	}
}

func TestServerSpanContinuesInboundTraceparent(t *testing.T) {
	tr, rec := newRecorded()
	var inner trace.SpanContext
	h := tr.Server("/v1/{op}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner = trace.SpanContextFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/query", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	h.ServeHTTP(httptest.NewRecorder(), req)
	s := rec.Ended()[0]
	if s.SpanContext().TraceID().String() != "0af7651916cd43dd8448eb211c80319c" || s.Parent().SpanID().String() != "b7ad6b7169203331" || !s.Parent().IsRemote() {
		t.Fatalf("trace=%s parent=%s", s.SpanContext().TraceID(), s.Parent().SpanID())
	}
	if inner.SpanID() != s.SpanContext().SpanID() {
		t.Fatal("the handler must run inside the server span")
	}
}

func TestSetupSpanEndsWhenStreamOpens(t *testing.T) {
	tr, rec := newRecorded()
	endedBeforeReturn := -1
	h := tr.Server("/v1/subscribe", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		endedBeforeReturn = len(rec.Ended())
	}), EndAtHeaders)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/subscribe", nil))
	if endedBeforeReturn != 1 {
		t.Fatalf("the subscribe span must end once the stream opens, ended=%d", endedBeforeReturn)
	}
	if len(rec.Ended()) != 1 {
		t.Fatalf("the span must end exactly once, got %d", len(rec.Ended()))
	}
}

func TestOffTracingIsTransparent(t *testing.T) {
	var off *Tracing
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if h := off.Server("/x", next); h == nil {
		t.Fatal("nil handler")
	}
	ctx, span := off.StartOp(context.Background(), "insert", "", "")
	span.SetScope("ns", "t")
	span.End("ok")
	if trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("off tracing must not create span contexts")
	}
	if New(nil, nil, false).On() {
		t.Fatal("a nil provider must mean off")
	}
}

func TestLogHandlerAddsTraceIDs(t *testing.T) {
	tr, _ := newRecorded()
	var buf bytes.Buffer
	log := slog.New(LogHandler(slog.NewJSONHandler(&buf, nil)))
	ctx, span := tr.StartOp(context.Background(), "insert", "", "")
	log.InfoContext(ctx, "inside")
	log.InfoContext(context.Background(), "outside")
	span.End("ok")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var in, out map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &in)
	_ = json.Unmarshal([]byte(lines[1]), &out)
	sc := span.span.SpanContext()
	if in["trace_id"] != sc.TraceID().String() || in["span_id"] != sc.SpanID().String() {
		t.Fatalf("log line inside a span = %v", in)
	}
	if _, ok := out["trace_id"]; ok {
		t.Fatalf("log line outside a span must not carry a trace id: %v", out)
	}
}

func TestPrincipalOnlyWhenOptedIn(t *testing.T) {
	for _, include := range []bool{false, true} {
		rec := tracetest.NewSpanRecorder()
		tr := New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)), nil, include)
		_, span := tr.StartOp(context.Background(), "insert", "", "alice")
		span.End("ok")
		got, ok := attrs(rec.Ended()[0])["enduser.id"]
		if ok != include || (include && got.AsString() != "alice") {
			t.Fatalf("include=%v: principal present=%v value=%v", include, ok, got.AsString())
		}
	}
}

func TestPeekScope(t *testing.T) {
	ns, tbl := PeekScope([]byte(`{"table":"notes","records":[{"namespace":"inner"}],"namespace":"app/eu"}`))
	if ns != "app/eu" || tbl != "notes" {
		t.Fatalf("ns=%q table=%q", ns, tbl)
	}
	ns, tbl = PeekScope([]byte(`{"namespace":7}`))
	if ns != "" || tbl != "" {
		t.Fatalf("non-string scope must be ignored: %q %q", ns, tbl)
	}
}
