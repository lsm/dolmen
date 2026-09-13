package dolmen

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func TestFacadeInputMatrixReadSearch(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	type call struct {
		name string
		fn   func(st *Store, ctx context.Context) error
		want error
	}
	calls := []call{
		{"get_rows: ids over max", func(st *Store, ctx context.Context) error {
			_, err := st.GetRows(ctx, "app", "notes", make([]int64, store.MaxReadRowsIDs+1))
			return err
		}, ErrInvalidRequest},
		{"get_rows: blank table", func(st *Store, ctx context.Context) error {
			_, err := st.GetRows(ctx, "app", "  ", []int64{1})
			return err
		}, ErrInvalidRequest},
		{"get_rows: blank namespace", func(st *Store, ctx context.Context) error {
			_, err := st.GetRows(ctx, "  ", "notes", []int64{1})
			return err
		}, ErrInvalidRequest},
		{"query: blank sql", func(st *Store, ctx context.Context) error {
			_, err := st.Query(ctx, "app", "   ", QueryOptions{})
			return err
		}, ErrInvalidRequest},
		{"query: non-read sql", func(st *Store, ctx context.Context) error {
			_, err := st.Query(ctx, "app", "DELETE FROM notes", QueryOptions{})
			return err
		}, ErrInvalidRequest},
		{"query: limit over max", func(st *Store, ctx context.Context) error {
			_, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{Limit: store.MaxPageLimit + 1})
			return err
		}, ErrInvalidRequest},
		{"query: negative limit", func(st *Store, ctx context.Context) error {
			_, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{Limit: -1})
			return err
		}, ErrInvalidRequest},
		{"query: negative offset", func(st *Store, ctx context.Context) error {
			_, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{Offset: -1})
			return err
		}, ErrInvalidRequest},
		{"query: args over max", func(st *Store, ctx context.Context) error {
			_, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{Args: make([]any, 101)})
			return err
		}, ErrInvalidRequest},
		{"search_fulltext: blank query", func(st *Store, ctx context.Context) error {
			_, err := st.SearchFulltext(ctx, "app", "notes", " \t ", SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"search_fulltext: blank table", func(st *Store, ctx context.Context) error {
			_, err := st.SearchFulltext(ctx, "app", "", "x", SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"search_fulltext: limit over max", func(st *Store, ctx context.Context) error {
			_, err := st.SearchFulltext(ctx, "app", "notes", "x", SearchOptions{Limit: store.MaxSearchLimit + 1})
			return err
		}, ErrInvalidRequest},
		{"search_fulltext: negative offset", func(st *Store, ctx context.Context) error {
			_, err := st.SearchFulltext(ctx, "app", "notes", "x", SearchOptions{Offset: -1})
			return err
		}, ErrInvalidRequest},
		{"search_fulltext: blank filter", func(st *Store, ctx context.Context) error {
			_, err := st.SearchFulltext(ctx, "app", "notes", "x", SearchOptions{Filter: "  "})
			return err
		}, ErrInvalidRequest},
		{"search_fulltext: semicolon filter", func(st *Store, ctx context.Context) error {
			_, err := st.SearchFulltext(ctx, "app", "notes", "x", SearchOptions{Filter: "a; b"})
			return err
		}, ErrInvalidRequest},
		{"search_fulltext: args over max", func(st *Store, ctx context.Context) error {
			_, err := st.SearchFulltext(ctx, "app", "notes", "x", SearchOptions{Args: make([]any, 101)})
			return err
		}, ErrInvalidRequest},
		{"search_vector: text plus non-empty vector", func(st *Store, ctx context.Context) error {
			_, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Text: "x", Vec: []float32{1}}, SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"search_vector: text plus empty vector", func(st *Store, ctx context.Context) error {
			_, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Text: "x", Vec: []float32{}}, SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"search_vector: empty vector alone", func(st *Store, ctx context.Context) error {
			_, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{}}, SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"search_vector: neither text nor vector", func(st *Store, ctx context.Context) error {
			_, err := st.SearchVector(ctx, "app", "notes", VectorQuery{}, SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"search_vector: NaN min score", func(st *Store, ctx context.Context) error {
			_, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{1}, MinScore: &nan}, SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"search_vector: infinite min score", func(st *Store, ctx context.Context) error {
			_, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{1}, MinScore: &inf}, SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"search_vector: NaN vector element", func(st *Store, ctx context.Context) error {
			_, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{float32(nan)}}, SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"search_vector: blank table", func(st *Store, ctx context.Context) error {
			_, err := st.SearchVector(ctx, "app", "  ", VectorQuery{Vec: []float32{1}}, SearchOptions{})
			return err
		}, ErrInvalidRequest},
		{"canceled context: every method", func(st *Store, ctx context.Context) error {
			if _, err := st.GetRows(ctx, "app", "notes", []int64{1}); !errors.Is(err, ErrCanceled) {
				return err
			}
			if _, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{}); !errors.Is(err, ErrCanceled) {
				return err
			}
			if _, err := st.SearchFulltext(ctx, "app", "notes", "x", SearchOptions{}); !errors.Is(err, ErrCanceled) {
				return err
			}
			if _, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{1}}, SearchOptions{}); !errors.Is(err, ErrCanceled) {
				return err
			}
			return nil
		}, nil},
	}
	for _, c := range calls {
		st, err := Open(t.TempDir())
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		ctx := context.Background()
		if c.name == "canceled context: every method" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}
		err = c.fn(st, ctx)
		if c.want == nil {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			}
		} else if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		live, lerr := st.ListNamespaces(context.Background(), ListNamespacesOptions{})
		if lerr != nil {
			t.Fatalf("%s: list namespaces: %v", c.name, lerr)
		}
		if len(live) != 0 {
			t.Errorf("%s: rejected call left a namespace behind: %v", c.name, live)
		}
		st.Close()
	}
}

func TestFacadeInputMatrixTableGrammar(t *testing.T) {
	for _, bad := range []string{"bad-name", "1starts", "has__fts", "sqlite_live", "over64characters_over64characters_over64characters_over64characters_x0", "with space"} {
		st, err := Open(t.TempDir())
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		ctx := context.Background()
		if _, err := st.GetRows(ctx, "app", bad, []int64{1}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("get_rows table %q: got %v, want invalid_request", bad, err)
		}
		if _, err := st.SearchFulltext(ctx, "app", bad, "x", SearchOptions{}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("fulltext table %q: got %v, want invalid_request", bad, err)
		}
		if _, err := st.SearchVector(ctx, "app", bad, VectorQuery{Vec: []float32{1}}, SearchOptions{}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("vector table %q: got %v, want invalid_request", bad, err)
		}
		live, lerr := st.ListNamespaces(ctx, ListNamespacesOptions{})
		if lerr != nil {
			t.Fatalf("list namespaces: %v", lerr)
		}
		if len(live) != 0 {
			t.Errorf("rejected calls for table %q left a namespace behind: %v", bad, live)
		}
		st.Close()
	}
}
