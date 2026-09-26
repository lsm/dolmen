package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type recordedLogs struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (r *recordedLogs) Export(_ context.Context, records []sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range records {
		r.records = append(r.records, rec.Clone())
	}
	return nil
}

func (r *recordedLogs) ForceFlush(context.Context) error { return nil }

func (r *recordedLogs) Shutdown(context.Context) error { return nil }

func (r *recordedLogs) bodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.records {
		out = append(out, rec.Body().AsString())
	}
	return out
}

func logFactory(called *bool, exp sdklog.Exporter) logExporterFactory {
	return func(context.Context) (sdklog.Exporter, error) {
		*called = true
		return exp, nil
	}
}

func TestLogsAreExportedOnlyWhenAskedFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"nothing set", map[string]string{}, false},
		{"an endpoint alone does not ship logs", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"}, false},
		{"explicit otlp", map[string]string{"OTEL_LOGS_EXPORTER": "otlp"}, true},
		{"explicit none", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318", "OTEL_LOGS_EXPORTER": "none"}, false},
		{"sdk disabled", map[string]string{"OTEL_LOGS_EXPORTER": "otlp", "OTEL_SDK_DISABLED": "true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			p, err := setup(context.Background(), envOf(tc.env), "v1",
				recordingFactory(new(bool), tracetest.NewInMemoryExporter()),
				withLogExporter(logFactory(&called, &recordedLogs{})))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Shutdown(context.Background())
			if called != tc.want {
				t.Fatalf("log exporter built = %v, want %v", called, tc.want)
			}
		})
	}
}

func TestUnsupportedLogSettingsAreRefusedWithTheFix(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"OTEL_LOGS_EXPORTER": "console"}, "otlp"},
		{map[string]string{"OTEL_LOGS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "grpc"}, "http/protobuf"},
	} {
		_, err := setup(context.Background(), envOf(tc.env), "v1",
			recordingFactory(new(bool), tracetest.NewInMemoryExporter()),
			withLogExporter(logFactory(new(bool), &recordedLogs{})))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("env %v: want an error mentioning %q, got %v", tc.env, tc.want, err)
		}
	}
}

func TestExportedLogsKeepStderrAndHonourTheLevel(t *testing.T) {
	rec := &recordedLogs{}
	p, err := setup(context.Background(), envOf(map[string]string{"OTEL_LOGS_EXPORTER": "otlp"}), "v1",
		recordingFactory(new(bool), tracetest.NewInMemoryExporter()),
		withLogExporter(logFactory(new(bool), rec)))
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	logger := slog.New(p.LogHandler(slog.NewTextHandler(&stderr, &slog.HandlerOptions{Level: slog.LevelInfo}), slog.LevelInfo))
	logger.Debug("below the level")
	logger.Info("namespace opened", "namespace", "app")
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "namespace opened") {
		t.Fatalf("stderr must keep every log line; got %q", stderr.String())
	}
	bodies := strings.Join(rec.bodies(), "|")
	if !strings.Contains(bodies, "namespace opened") {
		t.Fatalf("the info line must be exported; exported %q", bodies)
	}
	if strings.Contains(bodies+stderr.String(), "below the level") {
		t.Fatal("-log-level must gate both stderr and the exported logs")
	}
}

func TestExportedLogsCarryTheTraceContext(t *testing.T) {
	rec := &recordedLogs{}
	p, err := setup(context.Background(), envOf(map[string]string{"OTEL_LOGS_EXPORTER": "otlp"}), "v1",
		recordingFactory(new(bool), tracetest.NewInMemoryExporter()),
		withLogExporter(logFactory(new(bool), rec)))
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(context.Background(), "op")
	logger := slog.New(p.LogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil), slog.LevelInfo))
	logger.InfoContext(ctx, "inside a request")
	span.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.records) != 1 {
		t.Fatalf("want one exported record, got %d", len(rec.records))
	}
	if got := rec.records[0].TraceID(); got != span.SpanContext().TraceID() {
		t.Fatalf("exported log trace id = %s, want %s", got, span.SpanContext().TraceID())
	}
}
