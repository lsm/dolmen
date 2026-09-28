package postgres

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lsm/dolmen/internal/store"
)

func tracedPGStore(t *testing.T) (*Store, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	cfg := testConfig(t)
	cfg.TracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return openTest(t, cfg), rec
}

func pgSpanErrorType(spans []sdktrace.ReadOnlySpan, name string) (string, bool) {
	for _, s := range spans {
		if s.Name() != name {
			continue
		}
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "error.type" {
				return kv.Value.AsString(), true
			}
		}
		return "", false
	}
	return "", false
}

func pgEndedNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}

func TestPostgresTheStorageSpanTakesItsClassFromTheCallersContext(t *testing.T) {
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
			ctx:  func(t *testing.T) context.Context { return t.Context() },
			want: "invalid_request",
			why:  "with nothing to blame the context for, the error keeps the class it has always had",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, rec := tracedPGStore(t)
			seedPGStageTable(t, s, "spans", "docs", 1)
			ctx := c.ctx(t)
			rec.Reset()
			// An empty change list is refused before the engine touches the database, which is what
			// makes the case deterministic: the error is a classified invalid_request and it never
			// wraps the context's error, so only the context can decide this span's class.
			if _, err := s.Migrate(ctx, "spans", "docs", nil, store.Embedder{}, store.Incarnation{Version: 1}); err == nil {
				t.Fatal("a migrate with no changes must be refused")
			}
			got, ok := pgSpanErrorType(rec.Ended(), "MIGRATE docs")
			if !ok {
				t.Fatalf("the migrate span carries no error.type: %v", pgEndedNames(rec.Ended()))
			}
			if got != c.want {
				t.Fatalf("the storage span reports %q, want %q: %s", got, c.want, c.why)
			}
		})
	}
}

// TestPostgresACancelledQueryLeavesCanceledOnTheSpan is a guard rather than a failing-first test:
// queryError already maps a done context to the context's own error, so this path is right today.
// It is here so the two engines answer the same question the same way, since the rule this PR adds
// is that the context decides and the error only fills in the rest.
