package dolmen

import (
	"context"
	"errors"
	"github.com/lsm/dolmen/internal/store"
	"math"
	"testing"
)

func openWithSearch(t *testing.T, provider EmbeddingProvider) (*Store, context.Context) {
	t.Helper()
	st, err := Open(t.TempDir(), WithEmbedding(provider))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "app", "docs", []Field{
		{Name: "body", Type: Text, Fulltext: true, Vectorize: true},
		{Name: "grade", Type: Number},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return st, ctx
}

func countProvider(dim int) *recordingProvider {
	return &recordingProvider{identity: "fake|v1", dim: dim}
}

func ones(n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = 1
	}
	return v
}

func TestSearchFulltextFiltersAndRanks(t *testing.T) {
	st, ctx := openWithSearch(t, countProvider(8))
	if _, err := st.Insert(ctx, "app", "docs", []map[string]any{
		{"body": "payment refunded promptly", "grade": 1},
		{"body": "payment pending", "grade": 2},
		{"body": "unrelated text", "grade": 1},
	}, InsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := st.SearchFulltext(ctx, "app", "docs", "payments", SearchOptions{Filter: "grade = ?", Args: []any{1}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["body"] != "payment refunded promptly" {
		t.Fatalf("stemming and filtering must both apply, got %+v", res.Rows)
	}
}

func TestSearchVectorByTextEmbedsQuery(t *testing.T) {
	st, ctx := openWithSearch(t, countProvider(8))
	if _, err := st.Insert(ctx, "app", "docs", []map[string]any{{"body": "hello world"}}, InsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := st.SearchVector(ctx, "app", "docs", VectorQuery{Text: "hello"}, SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected the vectorized row, got %+v", res.Rows)
	}
	if _, has := res.Rows[0]["_score"]; !has {
		t.Fatal("results must carry _score")
	}
}

func TestSearchVectorRawVectorAndMinScore(t *testing.T) {
	st, ctx := openWithSearch(t, countProvider(8))
	if _, err := st.Insert(ctx, "app", "docs", []map[string]any{{"body": "hello world"}}, InsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := st.SearchVector(ctx, "app", "docs", VectorQuery{Vec: ones(8)}, SearchOptions{})
	if err != nil {
		t.Fatalf("raw vector search: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected a hit from a raw vector, got %+v", res.Rows)
	}
	below := 0.5
	kept, err := st.SearchVector(ctx, "app", "docs", VectorQuery{Vec: ones(8), MinScore: &below}, SearchOptions{})
	if err != nil {
		t.Fatalf("min_score search: %v", err)
	}
	if len(kept.Rows) != 1 {
		t.Fatalf("an identical vector clears any reachable min_score, got %+v", kept.Rows)
	}
	above := 1.5
	dropped, err := st.SearchVector(ctx, "app", "docs", VectorQuery{Vec: ones(8), MinScore: &above}, SearchOptions{})
	if err != nil {
		t.Fatalf("min_score search: %v", err)
	}
	if len(dropped.Rows) != 0 {
		t.Fatalf("an unreachable min_score must drop every row, got %+v", dropped.Rows)
	}
}

func TestSearchVectorRejectsTextWithoutProvider(t *testing.T) {
	dir := t.TempDir()
	seed, err := Open(dir, WithEmbedding(countProvider(8)))
	if err != nil {
		t.Fatalf("open with provider: %v", err)
	}
	ctx := context.Background()
	if _, err := seed.CreateTable(ctx, "app", "docs", []Field{{Name: "body", Type: Text, Vectorize: true}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := seed.Insert(ctx, "app", "docs", []map[string]any{{"body": "hello world"}}, InsertOptions{}); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen without provider: %v", err)
	}
	defer st.Close()
	_, err = st.SearchVector(ctx, "app", "docs", VectorQuery{Text: "hello"}, SearchOptions{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("text search without a provider must be invalid_request, got %v", err)
	}
	if res, err := st.SearchVector(ctx, "app", "docs", VectorQuery{Vec: ones(8)}, SearchOptions{}); err != nil || len(res.Rows) != 1 {
		t.Fatalf("raw vector search needs no provider, got %+v (%v)", res, err)
	}
}

func TestSearchVectorProviderFailureClassifiesAndKeepsCause(t *testing.T) {
	boom := errors.New("provider exploded")
	st, ctx := openWithSearch(t, &recordingProvider{identity: "fake|v1", dim: 8, fail: boom})
	_, err := st.SearchVector(ctx, "app", "docs", VectorQuery{Text: "hello"}, SearchOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("provider failures must keep their cause, got %v", err)
	}
	if !errors.Is(err, ErrEmbedderUnavailable) {
		t.Fatalf("a supplied provider's failure must classify as embedder_unavailable, got %v", err)
	}
}

func TestWritePathProviderFailureClassifiesUnavailable(t *testing.T) {
	boom := errors.New("provider exploded")
	st, err := Open(t.TempDir(), WithEmbedding(&recordingProvider{identity: "fake|v1", dim: 8, fail: boom}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "app", "docs", []Field{{Name: "body", Type: Text, Vectorize: true}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = st.Insert(ctx, "app", "docs", []map[string]any{{"body": "seed"}}, InsertOptions{})
	if !errors.Is(err, boom) || !errors.Is(err, ErrEmbedderUnavailable) {
		t.Fatalf("write-path provider failures must classify embedder_unavailable with the cause kept, got %v", err)
	}
}

func TestReadAndSearchHonorCanceledContexts(t *testing.T) {
	st, err := Open(t.TempDir(), WithEmbedding(&staticProvider{identity: "fake|v1"}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.GetRows(ctx, "app", "notes", []int64{1}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("get rows on a canceled context must classify canceled, got %v", err)
	}
	if _, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("query on a canceled context must classify canceled, got %v", err)
	}
	if _, err := st.SearchFulltext(ctx, "app", "notes", "x", SearchOptions{}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("fulltext search on a canceled context must classify canceled, got %v", err)
	}
	if _, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{1}}, SearchOptions{}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("vector search on a canceled context must classify canceled, got %v", err)
	}
	live, err := st.ListNamespaces(context.Background(), ListNamespacesOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("canceled reads must not leave a namespace behind, got %v", live)
	}
}

func TestSearchFulltextRejectsBlankQueryBeforeStorage(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	for _, blank := range []string{"", "   \t "} {
		if _, err := st.SearchFulltext(context.Background(), "app", "notes", blank, SearchOptions{}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("a blank query must be invalid_request before any storage work, got %v (%q)", err, blank)
		}
	}
	live, err := st.ListNamespaces(context.Background(), ListNamespacesOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("a rejected search must not leave a namespace behind, got %v", live)
	}
}

func TestReadAndSearchRejectOutOfRangeLimitsBeforeStorage(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	big := make([]int64, store.MaxReadRowsIDs+1)
	if _, err := st.GetRows(ctx, "app", "notes", big); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an oversized id list must be invalid_request before storage, got %v", err)
	}
	if _, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{Limit: store.MaxPageLimit + 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an out-of-range query limit must be invalid_request, got %v", err)
	}
	if _, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{Limit: -1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a negative query limit must be invalid_request, got %v", err)
	}
	if _, err := st.SearchFulltext(ctx, "app", "notes", "x", SearchOptions{Limit: store.MaxSearchLimit + 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an out-of-range search limit must be invalid_request, got %v", err)
	}
	if _, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{1}}, SearchOptions{Limit: -1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a negative search limit must be invalid_request, got %v", err)
	}
	live, err := st.ListNamespaces(ctx, ListNamespacesOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("rejected calls must not leave a namespace behind, got %v", live)
	}
}

func TestPreflightParityRejectsSchemaShapesBeforeStorage(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.Query(ctx, "app", "   ", QueryOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("blank sql must be invalid_request, got %v", err)
	}
	if _, err := st.Query(ctx, "app", "DELETE FROM notes", QueryOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("non-read sql must be invalid_request, got %v", err)
	}
	if _, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{Args: make([]any, 101)}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("too many args must be invalid_request, got %v", err)
	}
	if _, err := st.Query(ctx, "app", "SELECT 1", QueryOptions{Offset: -1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("negative offset must be invalid_request, got %v", err)
	}
	if _, err := st.SearchFulltext(ctx, "app", "notes", "x", SearchOptions{Filter: "a; b"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a semicolon in filter must be invalid_request, got %v", err)
	}
	if _, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{1}}, SearchOptions{Offset: -1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("negative search offset must be invalid_request, got %v", err)
	}
	live, err := st.ListNamespaces(ctx, ListNamespacesOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("rejected calls must not leave a namespace behind, got %v", live)
	}
}

func TestSearchVectorRejectsNonFiniteMinScore(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		ms := v
		if _, err := st.SearchVector(ctx, "app", "notes", VectorQuery{Vec: []float32{1}, MinScore: &ms}, SearchOptions{}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("a non-finite MinScore (%v) must be invalid_request, got %v", v, err)
		}
	}
	live, err := st.ListNamespaces(ctx, ListNamespacesOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("rejected searches must not leave a namespace behind, got %v", live)
	}
}
