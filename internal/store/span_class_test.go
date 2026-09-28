package store

import (
	"context"
	"errors"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func spanErrorTypeOf(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) (string, bool) {
	t.Helper()
	for _, s := range spans {
		if s.Name() != name {
			continue
		}
		if got := spanAttrOf(s, "error.type"); got != nil {
			code, _ := got.(string)
			return code, true
		}
		return "", false
	}
	return "", false
}

func endedNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}

func TestTheStorageSpanTakesItsClassFromTheCallersContext(t *testing.T) {
	for _, c := range []struct {
		name string
		ctx  func(t *testing.T) context.Context
		want string
		why  string
	}{
		{
			name: "cancelled",
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: "canceled",
			why:  "the caller went away, so the span must not report the error the request would have got anyway",
		},
		{
			name: "past its deadline",
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			want: "timeout",
			why:  "the caller's deadline passed, which is a timeout and not whatever the operation returned",
		},
		{
			name: "context still live",
			ctx:  func(t *testing.T) context.Context { return context.Background() },
			want: "invalid_request",
			why:  "with nothing to blame the context for, the error keeps the class it has always had",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := openTraced(t)
			ctx := c.ctx(t)
			// An empty change list is refused before the engine touches storage, which is what
			// makes the case deterministic: the error is a classified invalid_request and it never
			// wraps the context's error, so only the context can decide this span's class.
			if _, err := tr.st.Migrate(ctx, "test", "notes", nil, Embedder{}, Incarnation{Version: 1}); err == nil {
				t.Fatal("a migrate with no changes must be refused")
			}
			got, ok := spanErrorTypeOf(t, tr.rec.Ended(), "MIGRATE notes")
			if !ok {
				t.Fatalf("the migrate span carries no error.type: %v", endedNames(tr.rec.Ended()))
			}
			if got != c.want {
				t.Fatalf("the storage span reports %q, want %q: %s", got, c.want, c.why)
			}
		})
	}
}

func TestACancelledScanLeavesCanceledOnTheSpanNotQueryError(t *testing.T) {
	tr := openTraced(t)
	running := tr.hook.when("SELECT")
	// This is a guard rather than a failing-first test. When modernc notices the cancel, the
	// driver error is context.Canceled and the store maps it, so the span is already canceled. The
	// case it pins is the lost interrupt, where the driver reports its own error that does not wrap
	// context.Canceled: the classification has to come from the context anyway, or the span says
	// query_error and blames the caller's SQL for the caller going away.
	ctx, cancel := context.WithCancel(context.Background())
	answered := make(chan error, 1)
	go func() {
		_, err := tr.st.Query(ctx, "test", longRecursiveQuery, nil, [16]byte{}, Page{Limit: 10})
		answered <- err
	}()
	select {
	case <-running:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("the query never reached its SELECT span")
	}
	cancel()
	select {
	case err := <-answered:
		if err == nil {
			t.Fatal("a cancelled query returned no error")
		}
		// modernc may lose the interrupt and let a bounded statement finish, so either answer is
		// allowed. What matters is that the span does not blame the query in either case.
		if !errors.Is(err, context.Canceled) {
			t.Logf("the query answered %v rather than a cancellation, which modernc allows when the interrupt is lost", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a cancelled scan never returned")
	}
	got, ok := spanErrorTypeOf(t, tr.rec.Ended(), "SELECT")
	if !ok {
		t.Fatalf("the query span carries no error.type, so a failed query is invisible in the trace: %v", endedNames(tr.rec.Ended()))
	}
	if got != "canceled" {
		t.Fatalf("the storage span reports %q, want canceled: the caller cancelled a scan whose driver error does not wrap context.Canceled, and query_error here blames the caller's SQL for the caller going away", got)
	}
}
