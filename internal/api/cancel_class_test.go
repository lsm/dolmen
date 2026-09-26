package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

var errEngineInterrupt = errors.New("interrupted (9)")

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

func seedInterruptEngine(t *testing.T) store.Engine {
	t.Helper()
	eng, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	ctx := context.Background()
	if err := eng.CreateNamespace(ctx, "iv", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.CreateTable(ctx, "iv", "docs", []schema.Field{
		{Name: "title", Type: schema.Text, Fulltext: true},
		{Name: "body", Type: schema.Text, Vectorize: true},
	}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestACancelledRequestIsCanceledWhateverTheEngineSaid(t *testing.T) {
	eng := seedInterruptEngine(t)
	for _, c := range []struct {
		op   string
		body map[string]any
	}{
		{"read_rows", map[string]any{"namespace": "iv", "table": "docs", "ids": []int{1}}},
		{"search_fulltext", map[string]any{"namespace": "iv", "table": "docs", "query": "one"}},
		{"search_vector", map[string]any{"namespace": "iv", "table": "docs", "text": "one"}},
		{"query", map[string]any{"namespace": "iv", "sql": "SELECT title FROM docs"}},
	} {
		t.Run(c.op, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			srv := New(interruptEngine{Engine: eng, cancel: cancel}, fakeEmb{})
			raw, err := json.Marshal(c.body)
			if err != nil {
				t.Fatal(err)
			}
			_, err = srv.Dispatch(ctx, c.op, raw)
			if err == nil {
				t.Fatalf("a cancelled %s must fail", c.op)
			}
			got := WrapError(err)
			if got.Status != http.StatusOK || got.Code != ErrCodeCanceled {
				t.Fatalf("a cancelled %s answered %d %s: the caller is gone, so the engine's own error is not a server fault", c.op, got.Status, got.Code)
			}
			if !errors.Is(got, errEngineInterrupt) {
				t.Fatalf("a cancelled %s lost the engine's error: cause is %v, want the driver's own error kept for the log", c.op, got.Cause)
			}
		})
	}
}
