package store

import (
	"context"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func fakeEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		for _, r := range []byte(t) {
			v[r%8] += 1
		}
		out[i] = v
	}
	return out, nil
}

var testEmbed = Embedder{Embed: fakeEmbed, Identity: "fake-space"}

func mustInsertNotes(t *testing.T, st *Store) []int64 {
	t.Helper()
	ids, err := st.Insert(context.Background(), "test", "notes", []map[string]any{
		{"title": "first note", "body": "the dolmen stores stone tables", "score": 5, "done": true, "tags": []any{"a", "b"}, "emb": []any{1.0, 0, 0, 0}},
		{"title": "second note", "body": "agents keep their memory here", "score": 3, "done": false, "emb": []any{0, 1.0, 0, 0}},
		{"title": "third note", "body": "migration and schema evolution", "score": 1, "done": false, "emb": []any{0.9, 0.1, 0, 0}},
	}, testEmbed)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return ids
}

func TestInsertValidation(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "x", "bogus": 1},
	}, testEmbed); err == nil {
		t.Fatal("expected unknown field to be rejected")
	}

	fields := noteFields()
	fields[2].Required = true
	if _, err := st.CreateTable(ctx, "test", "req", fields); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "req", []map[string]any{
		{"title": "x"},
	}, testEmbed); err == nil {
		t.Fatal("expected missing required field to be rejected")
	}

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "x", "emb": []any{1.0, 0, 0}},
	}, testEmbed); err == nil {
		t.Fatal("expected wrong vector dim to be rejected")
	}

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "x", "score": "high"},
	}, testEmbed); err == nil {
		t.Fatal("expected wrong scalar type to be rejected")
	}
}

func TestCaseVariantKeyCollisionRejected(t *testing.T) {
	st := openStore(t)
	mustCreateNotes(t, st)
	if _, err := st.Insert(context.Background(), "test", "notes", []map[string]any{
		{"Title": "Alice", "title": "Bob"},
	}, testEmbed); err == nil {
		t.Fatal("expected case-variant key collision to be rejected")
	}
}

func TestProviderDimChangeRejected(t *testing.T) {
	st := openStore(t)
	if _, err := st.CreateTable(context.Background(), "test", "dims", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(context.Background(), "test", "dims", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	shortEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = make([]float32, 4)
		}
		return out, nil
	}
	shifted := Embedder{Embed: shortEmbed, Identity: "fake-space"}
	if _, err := st.Insert(context.Background(), "test", "dims", []map[string]any{{"s": "world"}}, shifted); err == nil {
		t.Fatal("expected same-identity dim change to be rejected")
	}
}

func TestVectorFloat32OverflowRejected(t *testing.T) {
	st := openStore(t)
	if _, err := st.CreateTable(context.Background(), "test", "of", []schema.Field{
		{Name: "v", Type: schema.Vector, Dim: 1},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(context.Background(), "test", "of", []map[string]any{{"v": []any{1e300}}}, testEmbed); err == nil {
		t.Fatal("expected out-of-float32-range vector entry to be rejected")
	}
}

func TestValidationRunsBeforeEmbedding(t *testing.T) {
	st := openStore(t)
	calls := 0
	counting := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		return fakeEmbed(ctx, texts)
	}, Identity: "fake-space"}
	if _, err := st.CreateTable(context.Background(), "test", "pree", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
		{Name: "n", Type: schema.Number},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(context.Background(), "test", "pree", []map[string]any{
		{"s": "valid text", "n": "not a number"},
	}, counting); err == nil {
		t.Fatal("expected invalid record to be rejected")
	}
	if calls != 0 {
		t.Fatalf("embedding provider must not be called for invalid records: %d calls", calls)
	}
}
