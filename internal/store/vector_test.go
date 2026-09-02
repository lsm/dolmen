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

	rows, _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 2)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(rows) != 2 || rows[0]["title"] != "first note" {
		t.Fatalf("unexpected vector results: %v", rows)
	}
	if score := rows[0]["_score"].(float64); score < 0.99 {
		t.Fatalf("expected cosine ~1, got %f", score)
	}

	if _, _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0}, "", 2); err == nil {
		t.Fatal("expected dim mismatch to be rejected")
	}

	qv, _ := fakeEmbed(ctx, []string{"the dolmen stores stone tables"})
	rows, _, err = st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 1)
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
	if _, _, err := st.SearchVector(ctx, "test", "auto", "", []float32{1, 0, 0, 0}, "", 5); err == nil {
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
	rows, truncated, err := st.SearchVector(ctx, "test", "vecbud", "emb", make([]float32, 4096), "", 200)
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
	rows, _, err := st.SearchVector(ctx, "test", "vlim", "emb", []float32{1, 0}, "", -1)
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
		if _, _, err := st.SearchVector(ctx, "test", "nfq", "emb", bad, "", 5); err == nil {
			t.Fatalf("expected non-finite query vector to be rejected: %v", bad)
		}
	}
}
