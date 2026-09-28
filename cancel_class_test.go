package dolmen

import (
	"context"
	"errors"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

var errEngineInterrupt = errors.New("interrupted (9)")

var errDeadlineEngine = errors.New("connection is busy")

type interruptEngine struct {
	store.Engine
	cancel context.CancelFunc
}

func (e interruptEngine) gone() error {
	e.cancel()
	return errEngineInterrupt
}

func (e interruptEngine) GetRows(ctx context.Context, ns, table string, ids []int64, scope *store.RowScope, inc store.Incarnation) (store.QueryResult, error) {
	return store.QueryResult{}, e.gone()
}

func (e interruptEngine) Query(ctx context.Context, ns, sql string, args []any, nsGen [16]byte, page store.Page) (store.QueryResult, error) {
	return store.QueryResult{}, e.gone()
}

func (e interruptEngine) SearchFulltext(ctx context.Context, ns, table, match, filter string, args []any, includeHidden bool, scope *store.RowScope, scopeIncarnation store.Incarnation, page store.Page) (store.SearchResult, error) {
	return store.SearchResult{}, e.gone()
}

func (e interruptEngine) SearchVector(ctx context.Context, ns, table string, q store.VectorQuery, includeHidden bool, scope *store.RowScope, scopeIncarnation store.Incarnation, page store.Page) (store.SearchResult, error) {
	return store.SearchResult{}, e.gone()
}

func TestACancelledCallIsCanceledWhateverTheEngineSaid(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		op   string
		span string
		call func(*Store, context.Context) error
	}{
		{"get_rows", "read_rows", func(st *Store, ctx context.Context) error {
			_, err := st.GetRows(ctx, "iv", "docs", []int64{1})
			return err
		}},
		{"query", "query", func(st *Store, ctx context.Context) error {
			_, err := st.Query(ctx, "iv", "SELECT title FROM docs", QueryOptions{})
			return err
		}},
		{"search_fulltext", "search_fulltext", func(st *Store, ctx context.Context) error {
			_, err := st.SearchFulltext(ctx, "iv", "docs", "one", SearchOptions{})
			return err
		}},
		{"search_vector", "search_vector", func(st *Store, ctx context.Context) error {
			_, err := st.SearchVector(ctx, "iv", "docs", VectorQuery{Vec: []float32{0.5, 0.25}}, SearchOptions{})
			return err
		}},
	} {
		t.Run(c.op, func(t *testing.T) {
			eng, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { eng.Close() })
			if err := eng.CreateNamespace(ctx, "iv", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.CreateTable(ctx, "iv", "docs", []schema.Field{
				{Name: "title", Type: schema.Text, Fulltext: true},
				{Name: "body", Type: schema.Text, Vectorize: true},
			}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(ctx)
			rec := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
			st, err := Open("", WithEngineOpener(store.EngineSQLite, "cancel-class-"+c.op, func(context.Context, time.Duration) (store.Engine, error) {
				return interruptEngine{Engine: eng, cancel: cancel}, nil
			}), WithTracerProvider(tp))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			rec.Reset()

			err = c.call(st, ctx)
			if err == nil {
				t.Fatalf("a cancelled %s must fail", c.op)
			}
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("a cancelled %s came back as %v, want ErrCanceled: the caller is gone, so the engine's own error is not a fault to report", c.op, err)
			}
			if !errors.Is(err, errEngineInterrupt) {
				t.Fatalf("a cancelled %s lost the engine's error: %v must stay reachable as the cause", c.op, err)
			}
			wantOpOutcome(t, rec, c.span, "canceled")
		})
	}
}

func TestAPastDeadlineIsTimeoutOnTheOperationSpan(t *testing.T) {
	ctx := context.Background()
	eng, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	if err := eng.CreateNamespace(ctx, "iv", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.CreateTable(ctx, "iv", "docs", []schema.Field{
		{Name: "body", Type: schema.Text},
	}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	st, err := Open("", WithEngineOpener(store.EngineSQLite, "deadline-class", func(context.Context, time.Duration) (store.Engine, error) {
		return &deadlineEngine{Engine: eng}, nil
	}), WithTracerProvider(tp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rec.Reset()

	past, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	_, err = st.GetRows(past, "iv", "docs", []int64{1})
	if !errors.Is(err, errDeadlineEngine) {
		t.Fatalf("a call past its deadline came back as %v, and the engine's own error must stay reachable: without that this test cannot tell a deadline passing during an engine call from the library answering before the engine was reached at all", err)
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a call whose deadline passed while the engine was answering came back as %v, want ErrTimeout: the engine answered with its own error, so only the context can have produced the timeout", err)
	}
	wantOpOutcome(t, rec, "read_rows", "timeout")
}

type deadlineEngine struct {
	store.Engine
}

func (e *deadlineEngine) GetRows(ctx context.Context, ns, table string, ids []int64, scope *store.RowScope, inc store.Incarnation) (store.QueryResult, error) {
	<-ctx.Done()
	return store.QueryResult{}, errDeadlineEngine
}

func wantOpOutcome(t *testing.T, rec *tracetest.SpanRecorder, op, want string) {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() != "dolmen.op "+op {
			continue
		}
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "dolmen.op.outcome" {
				if got := kv.Value.AsString(); got != want {
					t.Fatalf("the dolmen.op %s span records outcome %q, want %q", op, got, want)
				}
				return
			}
		}
		t.Fatalf("the dolmen.op %s span records no dolmen.op.outcome", op)
	}
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}
	t.Fatalf("no dolmen.op %s span was recorded, got %v", op, names)
}
