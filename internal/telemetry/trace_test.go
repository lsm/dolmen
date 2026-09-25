package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/mcp"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/telemetry"
)

const secret = "S3NSITIVE"

type fakeEmb struct{}

func (fakeEmb) Name() string      { return "fake" }
func (fakeEmb) ModelName() string { return "fake-model" }
func (fakeEmb) Identity() string  { return "fake-space" }
func (fakeEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		for _, r := range []byte(t) {
			v[r%8]++
		}
		out[i] = v
	}
	return out, nil
}
func (e fakeEmb) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	v, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

type harness struct {
	rec  *tracetest.SpanRecorder
	api  *api.Server
	http *httptest.Server
	mcp  *mcp.Server
}

func newHarness(t *testing.T, tracing bool) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rec := tracetest.NewSpanRecorder()
	var opts []api.Option
	if tracing {
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		opts = append(opts, api.WithTracing(telemetry.New(tp, propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}), false)))
	}
	a := api.New(st, fakeEmb{}, opts...)
	m := mcp.New(a, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", m)
	mux.Handle("/", a.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{rec: rec, api: a, http: srv, mcp: m}
}

func (h *harness) post(t *testing.T, path string, body any, header http.Header) int {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.http.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

func (h *harness) seed(t *testing.T) {
	t.Helper()
	if c := h.post(t, "/v1/create_table", map[string]any{"namespace": "app", "table": "notes", "fields": []map[string]any{{"name": "body", "type": "text", "vectorize": true, "fulltext": true}, {"name": "tag", "type": "string"}}}, nil); c != 200 {
		t.Fatalf("create_table: %d", c)
	}
}

func named(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func one(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	got := named(spans, name)
	if len(got) != 1 {
		var names []string
		for _, s := range spans {
			names = append(names, s.Name())
		}
		t.Fatalf("want one %q span, got %d among %v", name, len(got), names)
	}
	return got[0]
}

func attr(s sdktrace.ReadOnlySpan, key string) any {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsInterface()
		}
	}
	return nil
}

func childOf(child, parent sdktrace.ReadOnlySpan) bool {
	return child.Parent().SpanID() == parent.SpanContext().SpanID() && child.SpanContext().TraceID() == parent.SpanContext().TraceID()
}

func TestHTTPOpSpanTree(t *testing.T) {
	h := newHarness(t, true)
	h.seed(t)
	h.rec.Reset()
	if c := h.post(t, "/v1/insert", map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"body": "hello " + secret, "tag": secret}}}, nil); c != 200 {
		t.Fatalf("insert: %d", c)
	}
	spans := h.rec.Ended()
	server := one(t, spans, "POST /v1/{op}")
	op := one(t, spans, "dolmen.op insert")
	emb := one(t, spans, "embeddings fake-model")
	if server.SpanKind() != trace.SpanKindServer || op.SpanKind() != trace.SpanKindInternal || emb.SpanKind() != trace.SpanKindInternal {
		t.Fatalf("kinds: server=%v op=%v emb=%v", server.SpanKind(), op.SpanKind(), emb.SpanKind())
	}
	if !childOf(op, server) || !childOf(emb, op) {
		t.Fatal("want server -> op -> embeddings")
	}
	for k, v := range map[string]any{"dolmen.op.name": "insert", "dolmen.op.outcome": "ok", "db.namespace": "app", "dolmen.table": "notes"} {
		if attr(op, k) != v {
			t.Errorf("op %s = %v, want %v", k, attr(op, k), v)
		}
	}
	if id, _ := attr(op, "dolmen.request_id").(string); id == "" || attr(server, "dolmen.request_id") != id {
		t.Errorf("request id: op=%v server=%v", attr(op, "dolmen.request_id"), attr(server, "dolmen.request_id"))
	}
	if attr(op, "enduser.id") != nil {
		t.Error("principal must be off by default")
	}
	for k, v := range map[string]any{"gen_ai.operation.name": "embeddings", "gen_ai.request.model": "fake-model", "gen_ai.provider.name": "fake", "dolmen.embed.batch_size": int64(1)} {
		if attr(emb, k) != v {
			t.Errorf("embeddings %s = %v, want %v", k, attr(emb, k), v)
		}
	}
	for k, v := range map[string]any{"http.route": "/v1/{op}", "http.request.method": "POST", "http.response.status_code": int64(200), "url.scheme": "http"} {
		if attr(server, k) != v {
			t.Errorf("server %s = %v, want %v", k, attr(server, k), v)
		}
	}
}

func TestOpErrorStatus(t *testing.T) {
	h := newHarness(t, true)
	if c := h.post(t, "/v1/describe_table", map[string]any{"namespace": "app", "table": "missing"}, nil); c != 404 {
		t.Fatalf("describe_table: %d", c)
	}
	spans := h.rec.Ended()
	op := one(t, spans, "dolmen.op describe_table")
	if op.Status().Code != codes.Error || attr(op, "dolmen.op.outcome") != "not_found" || attr(op, "error.type") != "not_found" {
		t.Fatalf("status=%v outcome=%v", op.Status(), attr(op, "dolmen.op.outcome"))
	}
	server := one(t, spans, "POST /v1/{op}")
	if server.Status().Code == codes.Error || attr(server, "http.response.status_code") != int64(404) {
		t.Fatalf("a 4xx leaves the server span unset: %v %v", server.Status(), attr(server, "http.response.status_code"))
	}
}

func TestInboundTraceparentContinues(t *testing.T) {
	h := newHarness(t, true)
	hdr := http.Header{"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}
	h.post(t, "/v1/list_namespaces", map[string]any{}, hdr)
	h.post(t, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_namespaces", "arguments": map[string]any{}}}, hdr)
	for _, s := range h.rec.Ended() {
		if s.SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Errorf("%s left the inbound trace", s.Name())
		}
	}
	for _, name := range []string{"POST /v1/{op}", "POST /mcp"} {
		if s := one(t, h.rec.Ended(), name); s.Parent().SpanID().String() != "00f067aa0ba902b7" {
			t.Errorf("%s parent = %s", name, s.Parent().SpanID())
		}
	}
}

func TestMCPOpSpanTree(t *testing.T) {
	h := newHarness(t, true)
	h.seed(t)
	h.rec.Reset()
	h.post(t, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "search_vector", "arguments": map[string]any{"namespace": "app", "table": "notes", "text": "find " + secret}}}, nil)
	spans := h.rec.Ended()
	server := one(t, spans, "POST /mcp")
	op := one(t, spans, "dolmen.op search_vector")
	emb := one(t, spans, "embeddings fake-model")
	if !childOf(op, server) || !childOf(emb, op) {
		t.Fatal("want server -> op -> embeddings over MCP")
	}
	if attr(server, "http.route") != "/mcp" || attr(op, "dolmen.table") != "notes" {
		t.Fatalf("route=%v table=%v", attr(server, "http.route"), attr(op, "dolmen.table"))
	}
}

func TestMCPStdioOpSpan(t *testing.T) {
	h := newHarness(t, true)
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_namespace","arguments":{"namespace":"stdio"}}}` + "\n"
	var out bytes.Buffer
	if err := h.mcp.ServeStdio(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	op := one(t, h.rec.Ended(), "dolmen.op create_namespace")
	if attr(op, "db.namespace") != "stdio" || attr(op, "dolmen.op.outcome") != "ok" {
		t.Fatalf("attrs = %v", op.Attributes())
	}
}

func TestSubscribeSetupSpan(t *testing.T) {
	h := newHarness(t, true)
	h.post(t, "/v1/create_namespace", map[string]any{"namespace": "feed"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.http.URL+"/v1/subscribe?namespace=feed", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	defer cancel()
	if res.StatusCode != 200 {
		t.Fatalf("subscribe: %d", res.StatusCode)
	}
	s := one(t, h.rec.Ended(), "GET /v1/subscribe")
	if attr(s, "http.route") != "/v1/subscribe" || attr(s, "http.response.status_code") != int64(200) {
		t.Fatalf("attrs = %v", s.Attributes())
	}
}

func TestNoSensitiveValuesInSpans(t *testing.T) {
	h := newHarness(t, true)
	h.seed(t)
	calls := []struct {
		op   string
		body map[string]any
	}{
		{"insert", map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"body": "text " + secret, "tag": secret}}}},
		{"query", map[string]any{"namespace": "app", "sql": "SELECT * FROM notes WHERE tag = ? OR body = '" + secret + "'", "args": []any{secret}}},
		{"query", map[string]any{"namespace": "app", "sql": "SELECT broken " + secret}},
		{"update", map[string]any{"namespace": "app", "table": "notes", "filter": "tag = ?", "args": []any{secret}, "set": map[string]any{"tag": secret + "2"}}},
		{"search_fulltext", map[string]any{"namespace": "app", "table": "notes", "query": secret}},
		{"search_vector", map[string]any{"namespace": "app", "table": "notes", "text": secret, "filter": "tag = ?", "args": []any{secret}}},
		{"delete", map[string]any{"namespace": "app", "table": "notes", "filter": "tag = ?", "args": []any{secret}, "confirm": true}},
	}
	for _, c := range calls {
		h.post(t, "/v1/"+c.op, c.body, nil)
	}
	spans := h.rec.Ended()
	if len(spans) < 2*len(calls) {
		t.Fatalf("expected server, op and embedding spans, got %d", len(spans))
	}
	for _, s := range spans {
		check := []string{s.Name(), s.Status().Description}
		for _, kv := range s.Attributes() {
			check = append(check, kv.Value.Emit())
		}
		for _, e := range s.Events() {
			check = append(check, e.Name)
			for _, kv := range e.Attributes {
				check = append(check, kv.Value.Emit())
			}
		}
		for _, v := range check {
			if strings.Contains(v, secret) || strings.Contains(strings.ToUpper(v), "SELECT") {
				t.Errorf("span %q leaks %q", s.Name(), v)
			}
		}
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestLogLinesCarryTraceIDs(t *testing.T) {
	var buf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(telemetry.LogHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	t.Cleanup(func() { slog.SetDefault(prev) })
	h := newHarness(t, true)
	h.post(t, "/v1/describe_table", map[string]any{"namespace": "app", "table": "missing"}, nil)
	server := one(t, h.rec.Ended(), "POST /v1/{op}")
	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["request_id"] == nil {
			continue
		}
		if m["trace_id"] != server.SpanContext().TraceID().String() || m["span_id"] == nil {
			t.Errorf("request log line without the request's trace: %s", line)
		}
		found = true
	}
	if !found {
		t.Fatalf("no request log lines: %s", buf.String())
	}
}

func TestTracingOffExportsNothing(t *testing.T) {
	h := newHarness(t, false)
	h.seed(t)
	h.post(t, "/v1/insert", map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"body": "x"}}}, nil)
	h.post(t, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_namespaces", "arguments": map[string]any{}}}, nil)
	if n := len(h.rec.Ended()) + len(h.rec.Started()); n != 0 {
		t.Fatalf("tracing off recorded %d spans", n)
	}
	if h.api.Tracing().On() {
		t.Fatal("no tracing option must mean off")
	}
}

func TestOpenAIEmbeddingSpanInjectsTraceparent(t *testing.T) {
	var gotTP string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTP = r.Header.Get("traceparent")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]},{"index":1,"embedding":[0.3,0.4]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`))
	}))
	defer upstream.Close()
	rec := tracetest.NewSpanRecorder()
	tr := telemetry.New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)), propagation.TraceContext{}, false)
	p := tr.Embedder(&embed.OpenAI{BaseURL: upstream.URL, Model: "text-embedding-3-small", APIKey: "sk-" + secret})
	if _, err := p.Embed(context.Background(), []string{"a " + secret, "b"}); err != nil {
		t.Fatal(err)
	}
	s := one(t, rec.Ended(), "embeddings text-embedding-3-small")
	if s.SpanKind() != trace.SpanKindClient {
		t.Fatalf("kind = %v", s.SpanKind())
	}
	want := "00-" + s.SpanContext().TraceID().String() + "-" + s.SpanContext().SpanID().String() + "-01"
	if gotTP != want {
		t.Fatalf("traceparent = %q, want %q", gotTP, want)
	}
	if !bytes.Contains(gotBody, []byte(secret)) {
		t.Fatal("upstream must still receive the text")
	}
	for k, v := range map[string]any{"gen_ai.provider.name": "openai", "gen_ai.request.model": "text-embedding-3-small", "gen_ai.usage.input_tokens": int64(7), "dolmen.embed.batch_size": int64(2), "server.address": "127.0.0.1"} {
		if attr(s, k) != v {
			t.Errorf("%s = %v, want %v", k, attr(s, k), v)
		}
	}
	for _, kv := range s.Attributes() {
		if strings.Contains(kv.Value.Emit(), secret) {
			t.Errorf("%s leaks input or credentials", kv.Key)
		}
	}
}
