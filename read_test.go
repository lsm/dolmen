package dolmen

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type recordingProvider struct {
	identity string
	dim      int
	fail     error
}

func (p *recordingProvider) Identity() string { return p.identity }

func (p *recordingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	return p.dims(len(texts)), nil
}

func (p *recordingProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	return p.dims(1)[0], nil
}

func (p *recordingProvider) dims(n int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, p.dim)
		for j := range v {
			v[j] = 1
		}
		out[i] = v
	}
	return out
}

func TestGetRowsReturnsTypedValuesAndSkipsMissing(t *testing.T) {
	st, ctx := openWithNotes(t)
	ins, err := st.Insert(ctx, "app", "notes", []map[string]any{
		{"title": "a", "score": 1, "done": true, "tags": []any{"x", 1}},
		{"title": "b", "score": 2.5, "done": false, "tags": nil},
	}, InsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := st.GetRows(ctx, "app", "notes", []int64{ins.Ids[1], 404, ins.Ids[0]})
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}
	if len(res.Rows) != 2 || res.Truncated {
		t.Fatalf("missing ids are absent, never an error, got %+v", res)
	}
	if res.Rows[0]["title"] != "a" || res.Rows[1]["title"] != "b" {
		t.Fatalf("rows must come back in ascending id order, got %v", res.Rows)
	}
	if v, ok := res.Rows[0]["score"].(int64); !ok || v != 1 {
		t.Fatalf("integer numbers must read back as int64, got %T %v", res.Rows[0]["score"], res.Rows[0]["score"])
	}
	if v, ok := res.Rows[1]["score"].(float64); !ok || v != 2.5 {
		t.Fatalf("fractional numbers must read back as float64, got %T %v", res.Rows[1]["score"], res.Rows[1]["score"])
	}
	if v, ok := res.Rows[0]["done"].(bool); !ok || !v {
		t.Fatalf("booleans must come back as bool, got %T %v", res.Rows[0]["done"], res.Rows[0]["done"])
	}
	tags, ok := res.Rows[0]["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "x" {
		t.Fatalf("json fields must come back decoded, got %T %v", res.Rows[0]["tags"], res.Rows[0]["tags"])
	}
	if _, isNumber := tags[1].(json.Number); !isNumber || tags[1].(json.Number).String() != "1" {
		t.Fatalf("json numbers keep their encoded precision as json.Number, got %T %v", tags[1], tags[1])
	}
	if res.Rows[1]["tags"] != nil {
		t.Fatalf("omitted json fields must read back null, got %v", res.Rows[1]["tags"])
	}
	if _, has := res.Rows[0]["_embedding"]; has {
		t.Fatal("the hidden _embedding column must be omitted")
	}
}

func TestQueryPaginationAndArgs(t *testing.T) {
	st, ctx := openWithNotes(t)
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{
		{"title": "a", "score": 1},
		{"title": "b", "score": 2},
		{"title": "c", "score": 3},
	}, InsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	page, err := st.Query(ctx, "app", "SELECT title FROM notes WHERE score > ? ORDER BY score", QueryOptions{Args: []any{1}, Limit: 1})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Rows) != 1 || page.Rows[0]["title"] != "b" || !page.Truncated {
		t.Fatalf("expected one row of a larger result marked truncated, got %+v", page)
	}
	next, err := st.Query(ctx, "app", "SELECT title FROM notes WHERE score > ? ORDER BY score", QueryOptions{Args: []any{1}, Offset: 1, Limit: 1})
	if err != nil {
		t.Fatalf("query page two: %v", err)
	}
	if len(next.Rows) != 1 || next.Rows[0]["title"] != "c" || next.Truncated {
		t.Fatalf("expected the final row untruncated, got %+v", next)
	}
	past, err := st.Query(ctx, "app", "SELECT title FROM notes WHERE score > ? ORDER BY score", QueryOptions{Args: []any{1}, Offset: 2, Limit: 1})
	if err != nil {
		t.Fatalf("query page three: %v", err)
	}
	if len(past.Rows) != 0 || past.Truncated {
		t.Fatalf("expected an empty untruncated page past the end, got %+v", past)
	}
}

func TestQueryErrorClassification(t *testing.T) {
	st, ctx := openWithNotes(t)
	if _, err := st.Query(ctx, "app", "SELECT * FROM missing_table", QueryOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing table must be not_found, got %v", err)
	}
	if _, err := st.Query(ctx, "app", "INSERT INTO notes (title) VALUES ('x')", QueryOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a write statement must be rejected as invalid_request, got %v", err)
	}
	if _, err := st.Query(ctx, "app", "SELECT no_such_column FROM notes", QueryOptions{}); !errors.Is(err, ErrQuery) {
		t.Fatalf("a bad column must be a query error, got %v", err)
	}
}

func TestReadOperationsAfterCloseReturnErrClosed(t *testing.T) {
	st, ctx := openWithNotes(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := st.GetRows(ctx, "app", "notes", []int64{1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("get rows after close must return ErrClosed, got %v", err)
	}
	if _, err := st.Query(ctx, "app", "SELECT * FROM notes", QueryOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("query after close must return ErrClosed, got %v", err)
	}
	if _, err := st.SearchFulltext(ctx, "app", "notes", "a", SearchOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("search after close must return ErrClosed, got %v", err)
	}
	if _, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{1}}, SearchOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("vector search after close must return ErrClosed, got %v", err)
	}
}

func TestGetRowsLeavesCallerIdsUntouched(t *testing.T) {
	st, ctx := openWithNotes(t)
	ins, err := st.Insert(ctx, "app", "notes", []map[string]any{{"title": "a"}, {"title": "b"}}, InsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	ids := []int64{ins.Ids[1], ins.Ids[0]}
	before := append([]int64(nil), ids...)
	if _, err := st.GetRows(ctx, "app", "notes", ids); err != nil {
		t.Fatalf("get rows: %v", err)
	}
	for i := range before {
		if ids[i] != before[i] {
			t.Fatalf("GetRows must not reorder or rewrite the caller's slice: had %v, now %v", before, ids)
		}
	}
}
