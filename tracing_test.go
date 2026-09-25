package dolmen

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func spanAttr(s sdktrace.ReadOnlySpan, key string) any {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsInterface()
		}
	}
	return nil
}

func TestWithTracerProviderRecordsOpAndEmbeddingSpans(t *testing.T) {
	global := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	st, err := Open(t.TempDir(), WithEmbedding(countProvider(8)), WithTracerProvider(tp))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "App", "docs", []Field{{Name: "body", Type: Text, Vectorize: true}}); err != nil {
		t.Fatal(err)
	}
	rec.Reset()
	if _, err := st.Insert(ctx, "app", "docs", []map[string]any{{"body": "private words"}}, InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	var op, emb sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "dolmen.op insert":
			op = s
		case "embeddings":
			emb = s
		}
	}
	if op == nil || emb == nil {
		t.Fatalf("want op and embeddings spans, got %d spans", len(rec.Ended()))
	}
	if emb.Parent().SpanID() != op.SpanContext().SpanID() {
		t.Fatal("the embedding span must be a child of the op span")
	}
	if spanAttr(op, "db.namespace") != "app" || spanAttr(op, "dolmen.table") != "docs" || spanAttr(op, "dolmen.op.outcome") != "ok" {
		t.Fatalf("op attrs = %v", op.Attributes())
	}
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.Emit(), "private") {
				t.Errorf("%s leaks record text in %s", s.Name(), kv.Key)
			}
		}
	}
	rec.Reset()
	if _, _, err := st.DescribeTable(ctx, "app", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("describe missing: %v", err)
	}
	failed := rec.Ended()
	if len(failed) != 1 || failed[0].Status().Code != codes.Error || spanAttr(failed[0], "dolmen.op.outcome") != "not_found" {
		t.Fatalf("failed op spans = %v", failed)
	}
	if otel.GetTracerProvider() != global {
		t.Fatal("WithTracerProvider must not touch the global provider")
	}
}

func TestWithTracerProviderRejectsNil(t *testing.T) {
	var tp trace.TracerProvider
	if _, err := Open(t.TempDir(), WithTracerProvider(tp)); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "WithTracerProvider") {
		t.Fatalf("err = %v", err)
	}
}
