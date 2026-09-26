package conformance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lsm/dolmen/internal/store"
)

const (
	upstreamTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	upstreamSpanID  = "00f067aa0ba902b7"
	insertedText    = "a private sentence the tracer must never see"
)

func TestOneRequestProducesTheWholeSpanTree(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	h := newTracedHarness(t, tp)
	h.seedTable("tree", "docs", []map[string]any{{"name": "body", "type": "text", "vectorize": true}})
	rec.Reset()

	body, err := json.Marshal(map[string]any{
		"namespace": "tree",
		"table":     "docs",
		"records":   []map[string]any{{"body": insertedText}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := h.postWithHeaders(h.httpURL+"/insert", body, map[string]string{
		"traceparent": fmt.Sprintf("00-%s-%s-01", upstreamTraceID, upstreamSpanID),
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("insert failed: %d", res.StatusCode)
	}

	spans := rec.Ended()
	byParent := map[trace.SpanID][]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byParent[s.Parent().SpanID()] = append(byParent[s.Parent().SpanID()], s)
	}
	var servers []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "POST /v1/{op}" {
			servers = append(servers, s)
		}
	}
	if len(servers) != 1 {
		t.Fatalf("one request must produce one server span, got %d: %v", len(servers), spanNames(spans))
	}
	server := servers[0]
	if server.SpanKind() != trace.SpanKindServer {
		t.Errorf("the server span kind = %v, want server", server.SpanKind())
	}
	if got := server.SpanContext().TraceID().String(); got != upstreamTraceID {
		t.Errorf("trace id = %s, want the inbound %s: the server span must continue a traceparent", got, upstreamTraceID)
	}
	if got := server.Parent().SpanID().String(); got != upstreamSpanID {
		t.Errorf("the server span's parent = %s, want the inbound span %s", got, upstreamSpanID)
	}

	op := onlyChild(t, byParent[server.SpanContext().SpanID()], "dolmen.op insert")
	if got := attrOf(op, "dolmen.op.outcome"); got != "ok" {
		t.Errorf("dolmen.op.outcome = %q, want ok", got)
	}
	insert := onlyChild(t, byParent[op.SpanContext().SpanID()], "INSERT docs")
	if got := attrOf(insert, "db.operation.name"); got != "INSERT" {
		t.Errorf("db.operation.name = %q, want INSERT", got)
	}
	embedSpan := onlyChild(t, byParent[insert.SpanContext().SpanID()], "embeddings fake-model")
	if got := attrOf(embedSpan, "gen_ai.request.model"); got != "fake-model" {
		t.Errorf("gen_ai.request.model = %q, want fake-model", got)
	}
	if testEngine(t) == store.EnginePostgres {
		if extra := unexpectedChildren(t, byParent[insert.SpanContext().SpanID()], "embeddings fake-model"); len(extra) > 0 {
			t.Errorf("PostgreSQL has no per-namespace writer, but INSERT recorded %v", extra)
		}
	} else {
		for _, name := range []string{"dolmen.writer.wait", "dolmen.transaction"} {
			found := false
			for _, s := range byParent[insert.SpanContext().SpanID()] {
				if s.Name() == name {
					found = true
				}
			}
			if !found {
				t.Errorf("SQLite records %s under INSERT docs, but it is missing", name)
			}
		}
		tx := childNamed(t, byParent[insert.SpanContext().SpanID()], "dolmen.transaction")
		if got := attrOf(tx, "dolmen.tx.outcome"); got != "commit" {
			t.Errorf("dolmen.tx.outcome = %q, want commit", got)
		}
		if embedSpan.EndTime().After(tx.StartTime()) {
			t.Error("the embedding span must end before the write transaction starts, so a provider call never holds the writer")
		}
	}
	ensure := onlyChild(t, byParent[op.SpanContext().SpanID()], "CREATE")
	if got := attrOf(ensure, "db.namespace"); got != "tree" {
		t.Errorf("the namespace-ensure span names %q, want tree", got)
	}
	if got := attrOf(ensure, "db.collection.name"); got != "" {
		t.Errorf("the namespace-ensure span names the table %q, want none", got)
	}
	if extra := unexpectedChildren(t, byParent[ensure.SpanContext().SpanID()]); len(extra) > 0 {
		t.Errorf("the namespace-ensure span has children %v, want none", extra)
	}
	for _, s := range spans {
		if s.SpanContext().TraceID().String() != upstreamTraceID {
			t.Errorf("%s is in trace %s, want the request's single trace", s.Name(), s.SpanContext().TraceID())
		}
		for _, kv := range s.Attributes() {
			if v, _ := kv.Value.AsInterface().(string); strings.Contains(v, insertedText) {
				t.Errorf("%s leaks the inserted text in %s = %q", s.Name(), kv.Key, v)
			}
		}
		if strings.Contains(s.Status().Description, insertedText) {
			t.Errorf("%s leaks the inserted text in its status", s.Name())
		}
	}
}

func onlyChild(t *testing.T, children []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, s := range children {
		if s.Name() != name {
			continue
		}
		if found != nil {
			t.Fatalf("more than one %q span under the same parent", name)
		}
		found = s
	}
	if found == nil {
		t.Fatalf("no %q span where the tree requires one; recorded %v", name, spanNames(children))
	}
	return found
}

func childNamed(t *testing.T, children []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range children {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no %q span; recorded %v", name, spanNames(children))
	return nil
}

func unexpectedChildren(t *testing.T, children []sdktrace.ReadOnlySpan, want ...string) []string {
	t.Helper()
	var out []string
	for _, s := range children {
		known := false
		for _, w := range want {
			if s.Name() == w {
				known = true
			}
		}
		if !known {
			out = append(out, s.Name())
		}
	}
	return out
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	sort.Strings(out)
	return out
}

func attrOf(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			v, _ := kv.Value.AsInterface().(string)
			return v
		}
	}
	return ""
}
