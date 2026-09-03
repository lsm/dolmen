package store

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestVectorSearch(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	rows, _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 2, false, "", nil, nil)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(rows) != 2 || rows[0]["title"] != "first note" {
		t.Fatalf("unexpected vector results: %v", rows)
	}
	if score := rows[0]["_score"].(float64); score < 0.99 {
		t.Fatalf("expected cosine ~1, got %f", score)
	}

	if _, _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0}, "", 2, false, "", nil, nil); err == nil {
		t.Fatal("expected dim mismatch to be rejected")
	}

	qv, _ := fakeEmbed(ctx, []string{"the dolmen stores stone tables"})
	rows, _, err = st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 1, false, "", nil, nil)
	if err != nil {
		t.Fatalf("vectorize search: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "first note" {
		t.Fatalf("unexpected vectorize results: %v", rows)
	}
}

func TestRawVectorDimMismatchOnAutoEmbedding(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "auto", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "auto", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := st.SearchVector(ctx, "test", "auto", "", []float32{1, 0, 0, 0}, "", 5, false, "", nil, nil); err == nil {
		t.Fatal("expected wrong-length raw vector against auto-embeddings to be rejected")
	}
}

func TestValidateVectorSearchBeforeEmbedding(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if err := st.ValidateVectorSearch(ctx, "test", "missing", "", "fake-space"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not-found for missing table, got %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "vv", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "vv", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.ValidateVectorSearch(ctx, "test", "vv", "", "other-space"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected identity mismatch to be caught before embedding, got %v", err)
	}
}

func TestVectorDecorationBudgeted(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "vecbud", []schema.Field{
		{Name: "t", Type: schema.Text},
		{Name: "emb", Type: schema.Vector, Dim: 4096},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	bigVec := make([]any, 4096)
	for i := range bigVec {
		bigVec[i] = float64(1)
	}
	chunk := strings.Repeat("z", 160<<10)
	for b := 0; b < 2; b++ {
		records := make([]map[string]any, 0, 100)
		for i := 0; i < 100; i++ {
			records = append(records, map[string]any{"t": chunk, "emb": bigVec})
		}
		if _, err := st.Insert(ctx, "test", "vecbud", records, testEmbed); err != nil {
			t.Fatalf("insert %d: %v", b, err)
		}
	}
	rows, truncated, err := st.SearchVector(ctx, "test", "vecbud", "emb", make([]float32, 4096), "", 200, false, "", nil, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !truncated || len(rows) >= 150 {
		t.Fatalf("decorated vectors must count against the budget: %d rows truncated=%v", len(rows), truncated)
	}
}

func TestSearchVectorLimitBounded(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "vlim", []schema.Field{
		{Name: "emb", Type: schema.Vector, Dim: 2},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	records := make([]map[string]any, 0, 300)
	for i := 0; i < 300; i++ {
		records = append(records, map[string]any{"emb": []any{1, float64(i%50) / 50}})
	}
	if _, err := st.Insert(ctx, "test", "vlim", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.SearchVector(ctx, "test", "vlim", "emb", []float32{1, 0}, "", -1, false, "", nil, nil)
	if err != nil {
		t.Fatalf("search with negative limit must be bounded, not error: %v", err)
	}
	if len(rows) > 200 {
		t.Fatalf("negative limit must clamp to 200, got %d rows", len(rows))
	}
}

func TestSearchVectorRejectsNonFiniteQuery(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "nfq", []schema.Field{
		{Name: "emb", Type: schema.Vector, Dim: 3},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "nfq", []map[string]any{{"emb": []any{1, 2, 3}}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for _, bad := range [][]float32{{1, 2, float32(math.NaN())}, {1, 2, float32(math.Inf(-1))}} {
		if _, _, err := st.SearchVector(ctx, "test", "nfq", "emb", bad, "", 5, false, "", nil, nil); err == nil {
			t.Fatalf("expected non-finite query vector to be rejected: %v", bad)
		}
	}
}

func TestSearchVectorFilter(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	// Filter to rows whose title matches a bound parameter.
	rows, _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "title = ?", []any{"first note"}, nil)
	if err != nil {
		t.Fatalf("filtered vector search: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "first note" {
		t.Fatalf("expected one filtered hit, got %v", rows)
	}

	// Filter on a numeric metadata column.
	rows, _, err = st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "score >= 3", nil, nil)
	if err != nil {
		t.Fatalf("numeric filter search: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 hits with score >= 3, got %v", rows)
	}

	// Filter with no matches returns empty, not an error.
	rows, _, err = st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "title = ?", []any{"missing"}, nil)
	if err != nil {
		t.Fatalf("zero-match filter search: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 hits for missing filter, got %v", rows)
	}

	// Malicious/invalid filters are rejected like delete's filter.
	if _, _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "1=1; DROP TABLE notes", nil, nil); err == nil {
		t.Fatal("expected semicolon in filter to be rejected")
	}
	if _, _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "title = ? AND score > ?", []any{"first note"}, nil); err == nil {
		t.Fatal("expected too few filter args to be rejected")
	}
}

func TestSearchVectorMinScore(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	q := []float32{1, 0, 0, 0}
	// No threshold: all three non-null rows come back.
	rows, _, err := st.SearchVector(ctx, "test", "notes", "emb", q, "", 10, false, "", nil, nil)
	if err != nil {
		t.Fatalf("unfiltered search: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 hits without threshold, got %v", rows)
	}

	// Threshold just below the top score: only first and third notes.
	cut := 0.95
	rows, _, err = st.SearchVector(ctx, "test", "notes", "emb", q, "", 10, false, "", nil, &cut)
	if err != nil {
		t.Fatalf("min_score search: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 hits with min_score 0.95, got %v", rows)
	}
	for _, r := range rows {
		if r["_score"].(float64) < cut {
			t.Fatalf("result score %v below min_score %v", r["_score"], cut)
		}
	}

	// Threshold above the top score: empty.
	high := 1.01
	rows, _, err = st.SearchVector(ctx, "test", "notes", "emb", q, "", 10, false, "", nil, &high)
	if err != nil {
		t.Fatalf("high min_score search: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 hits above perfect score, got %v", rows)
	}

	// Threshold and filter together.
	cut = 0.95
	rows, _, err = st.SearchVector(ctx, "test", "notes", "emb", q, "", 10, false, "score >= 3", nil, &cut)
	if err != nil {
		t.Fatalf("filter + min_score search: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "first note" {
		t.Fatalf("expected first note only, got %v", rows)
	}
}
