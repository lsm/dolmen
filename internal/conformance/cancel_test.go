package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lsm/dolmen/internal/mcp"
)

const cancelGrace = 30 * time.Second

type startHook struct {
	sdktrace.SpanProcessor
	mu     sync.Mutex
	onName map[string]chan struct{}
}

func (h *startHook) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	h.mu.Lock()
	if ch, ok := h.onName[s.Name()]; ok {
		delete(h.onName, s.Name())
		close(ch)
	}
	h.mu.Unlock()
	h.SpanProcessor.OnStart(ctx, s)
}

func (h *startHook) when(name string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{})
	h.onName[name] = ch
	return ch
}

type servedResponse struct {
	status int
	env    map[string]any
}

func (h *harness) serveWithContext(ctx context.Context, handler http.Handler, path string, body any, contentType string) servedResponse {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("marshal %s body: %v", path, err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	handler.ServeHTTP(rec, req)
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return servedResponse{status: rec.Code, env: env}
}

func (h *harness) cancelledEnvelope(ctx context.Context, op string, args map[string]any) map[string]any {
	h.t.Helper()
	res := h.serveWithContext(ctx, mcp.New(h.api, nil), "/mcp", map[string]any{
		"jsonrpc": "2.0",
		"id":      mcpNextID(),
		"method":  "tools/call",
		"params":  map[string]any{"name": op, "arguments": args},
	}, "application/json")
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(mustJSON(h.t, res.env)), &rpc); err != nil {
		h.t.Fatalf("decode the cancelled %s over MCP: %v", op, err)
	}
	if len(rpc.Result.Content) == 0 {
		h.t.Fatalf("a cancelled %s over MCP answered with no content: %v", op, res.env)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &env); err != nil {
		h.t.Fatalf("decode the %s tool error over MCP: %v", op, err)
	}
	return env
}

func TestACancelledReadIsCanceledOverEveryTransport(t *testing.T) {
	h := newHarness(t)
	h.seedTable("cancel", "docs", []map[string]any{
		{"name": "title", "type": "text", "fulltext": true},
		{"name": "body", "type": "text", "vectorize": true},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "cancel", "table": "docs",
		"records": []map[string]any{{"title": "refund processed", "body": "refund processed"}},
	})

	for _, c := range []struct {
		op   string
		body map[string]any
	}{
		{"read_rows", map[string]any{"namespace": "cancel", "table": "docs", "ids": []int{1}}},
		{"search_fulltext", map[string]any{"namespace": "cancel", "table": "docs", "query": "refund"}},
		{"search_vector", map[string]any{"namespace": "cancel", "table": "docs", "text": "refund processed"}},
		{"query", map[string]any{"namespace": "cancel", "sql": "SELECT title FROM docs"}},
	} {
		t.Run(c.op, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			res := h.serveWithContext(ctx, h.api.Handler(), "/v1/"+c.op, c.body, "application/json")
			if res.status != http.StatusOK {
				t.Fatalf("a cancelled %s answered %d %v, want 200", c.op, res.status, res.env)
			}
			code, msg := errorOf(t, res.env)
			if code != "canceled" {
				t.Fatalf("a cancelled %s answered %q %q, want canceled", c.op, code, msg)
			}
			if !strings.Contains(msg, "check with a query before retrying") {
				t.Fatalf("the cancellation must say the operation may or may not have run: %q", msg)
			}
			env := h.cancelledEnvelope(ctx, c.op, c.body)
			if code, _ := env["code"].(string); code != "canceled" {
				t.Fatalf("MCP must carry the same cancellation as /v1, got %v", env)
			}
		})
	}
}

func TestACancelledQueryIsAnsweredAsCanceled(t *testing.T) {
	hook := &startHook{SpanProcessor: tracetest.NewSpanRecorder(), onName: map[string]chan struct{}{}}
	h := newTracedHarness(t, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(hook)))
	h.ensureNS("cancel")
	h.mustHTTP("query", map[string]any{"namespace": "cancel", "sql": "SELECT 1 AS one"})

	body, err := json.Marshal(map[string]any{"namespace": "cancel", "sql": endlessQuery})
	if err != nil {
		t.Fatal(err)
	}
	running := hook.when("SELECT")
	ctx, cancel := context.WithCancel(context.Background())
	answered := make(chan servedResponse, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		h.api.Handler().ServeHTTP(rec, req)
		var env map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		answered <- servedResponse{status: rec.Code, env: env}
	}()

	select {
	case <-running:
	case <-time.After(cancelGrace):
		cancel()
		select {
		case <-answered:
		case <-time.After(cancelGrace):
		}
		t.Fatalf("the query never reached the engine within %s", cancelGrace)
	}
	cancel()

	var got servedResponse
	select {
	case got = <-answered:
	case <-time.After(cancelGrace):
		t.Fatalf("a cancelled query never answered within %s: the caller's cancellation must stop the statement", cancelGrace)
	}
	h.mustHTTP("query", map[string]any{"namespace": "cancel", "sql": "SELECT 1 AS one"})

	if got.status == http.StatusOK && got.env["ok"] == true {
		t.Fatalf("a query that cannot finish answered as complete: %v", got.env)
	}
	code, msg := errorOf(t, got.env)
	if got.status != http.StatusOK || code != "canceled" {
		t.Fatalf("a cancelled query answered %d %s %q, want 200 canceled: a statement the caller interrupted is a caller that went away, not a server fault", got.status, code, msg)
	}
	if !strings.Contains(msg, "check with a query before retrying") {
		t.Fatalf("the cancellation must say the operation may or may not have run: %q", msg)
	}
}
