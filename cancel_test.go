package dolmen

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

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

func TestACancelledQueryComesBackAsCanceled(t *testing.T) {
	hook := &startHook{SpanProcessor: discardProcessor{}, onName: map[string]chan struct{}{}}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(hook))
	st, err := Open(t.TempDir(), WithTracerProvider(tp))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "cancel", "notes", []Field{{Name: "n", Type: Number}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "cancel", "notes", []map[string]any{{"n": 1}}, InsertOptions{}); err != nil {
		t.Fatal(err)
	}

	running := hook.when("SELECT")
	queryCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := st.Query(queryCtx, "cancel", "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 2000000) SELECT count(*) FROM c", QueryOptions{})
		done <- err
	}()
	select {
	case <-running:
	case <-time.After(30 * time.Second):
		t.Fatal("the query never reached the engine")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled query must fail")
		}
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("a cancelled query came back as %v, want ErrCanceled: the caller's cancellation is not a query error", err)
		}
		if errors.Is(err, ErrQuery) {
			t.Fatalf("a cancelled query must not be reported as a query error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the cancelled query never returned")
	}
	if _, err := st.Query(ctx, "cancel", "SELECT count(*) AS c FROM notes", QueryOptions{}); err != nil {
		t.Fatalf("the store must serve a normal query after a cancelled one: %v", err)
	}
}

type discardProcessor struct{}

func (discardProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (discardProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (discardProcessor) Shutdown(context.Context) error                  { return nil }
func (discardProcessor) ForceFlush(context.Context) error                { return nil }
