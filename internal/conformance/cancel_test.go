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
