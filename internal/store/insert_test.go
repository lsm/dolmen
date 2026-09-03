package store

import (
	"context"
	"math"
	"testing"
	"time"

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

func TestInsertRejectsEmptyIdentityAgainstRecordedSpace(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "anon", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "anon", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	faceless := Embedder{Embed: fakeEmbed, Identity: ""}
	if _, err := st.Insert(ctx, "test", "anon", []map[string]any{{"s": "more"}}, faceless); err == nil {
		t.Fatal("expected empty identity against a recorded embedding space to be rejected")
	}
}

func TestInsertRejectsZeroDimEmbeddings(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "zero", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	empty := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{}
		}
		return out, nil
	}, Identity: "empty-space"}
	if _, err := st.Insert(ctx, "test", "zero", []map[string]any{{"s": "a"}}, empty); err == nil {
		t.Fatal("expected all-empty embedding response to be rejected")
	}
	mixed := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		return [][]float32{{}, {1, 2, 3, 4, 5, 6, 7, 8}}, nil
	}, Identity: "mixed-space"}
	if _, err := st.Insert(ctx, "test", "zero", []map[string]any{{"s": "a"}, {"s": "b"}}, mixed); err == nil {
		t.Fatal("expected empty vector followed by a non-empty one to be rejected")
	}
}

func TestInsertAcceptsInferredNumericKinds(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	type score int32
	samples := []map[string]any{
		{"n": int32(7), "u": uint64(9), "f": float32(1.5), "d": score(3)},
	}
	fields := schema.InferFields(samples)
	if _, err := st.CreateTable(ctx, "test", "kinds", fields); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "kinds", samples, testEmbed); err != nil {
		t.Fatalf("inserting the same records inference accepted must work: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "kinds", []map[string]any{
		{"u": uint64(math.MaxUint64)},
	}, testEmbed); err == nil {
		t.Fatal("expected uint64 overflow of int64 to be rejected")
	}
}

func TestInsertRequiresIdentityForFirstEmbedding(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "firstid", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	faceless := Embedder{Embed: fakeEmbed, Identity: ""}
	if _, err := st.Insert(ctx, "test", "firstid", []map[string]any{{"s": "hello"}}, faceless); err == nil {
		t.Fatal("expected the first vectorized insert to require a provider identity")
	}
}

func TestInsertRejectsNonFiniteProviderVectors(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "nfv", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	nan := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		return [][]float32{{0, 1, float32(math.NaN()), 0, 0, 0, 0, 0}}, nil
	}, Identity: "nan-space"}
	if _, err := st.Insert(ctx, "test", "nfv", []map[string]any{{"s": "hello"}}, nan); err == nil {
		t.Fatal("expected NaN embedding component to be rejected")
	}
	inf := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		return [][]float32{{0, 1, float32(math.Inf(1)), 0, 0, 0, 0, 0}}, nil
	}, Identity: "inf-space"}
	if _, err := st.Insert(ctx, "test", "nfv", []map[string]any{{"s": "hello"}}, inf); err == nil {
		t.Fatal("expected infinite embedding component to be rejected")
	}
}

func TestInsertAcceptsInferredStringRepresentations(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	type label string
	type flag bool
	ptr := "pointed"
	samples := []map[string]any{
		{"l": label("named"), "p": &ptr, "w": time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC), "f": flag(true)},
	}
	fields := schema.InferFields(samples)
	if _, err := st.CreateTable(ctx, "test", "strs", fields); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "strs", samples, testEmbed); err != nil {
		t.Fatalf("inserting the representations inference accepted must work: %v", err)
	}
}

func TestInsertRejectsInvalidTimestamps(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "ts", []schema.Field{
		{Name: "at", Type: schema.Timestamp},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, bad := range []string{"not a timestamp", "2026-99-99", "2026-02-30", "hello world"} {
		if _, err := st.Insert(ctx, "test", "ts", []map[string]any{{"at": bad}}, testEmbed); err == nil {
			t.Fatalf("expected %q to be rejected as invalid timestamp", bad)
		}
	}

	if _, err := st.Insert(ctx, "test", "ts", []map[string]any{{"at": 123}}, testEmbed); err == nil {
		t.Fatal("expected non-string timestamp value to be rejected")
	}
}

func TestInsertAcceptsValidTimestampVariants(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "ts", []schema.Field{
		{Name: "at", Type: schema.Timestamp},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, good := range []string{
		"2026-09-01",
		"2026-09-01T10:00:00Z",
		"2026-09-01T10:00:00.5+02:00",
		"2026-09-01 10:00:00",
		"2026-09-01t10:00:00z",
		"2026-09-01T10:00:05",
	} {
		if _, err := st.Insert(ctx, "test", "ts", []map[string]any{{"at": good}}, testEmbed); err != nil {
			t.Fatalf("expected %q to be accepted as timestamp: %v", good, err)
		}
	}
}

func TestInsertAcceptsTimeTimeValuesForTimestamp(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "ts", []schema.Field{
		{Name: "at", Type: schema.Timestamp},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := st.Insert(ctx, "test", "ts", []map[string]any{
		{"at": time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},
	}, testEmbed); err != nil {
		t.Fatalf("expected time.Time value to be accepted: %v", err)
	}
}

func TestInsertAcceptsMarshalableJSONValues(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	samples := []map[string]any{
		{"tags": []string{"a", "b"}, "meta": map[string]string{"k": "v"}, "st": struct{ N int }{N: 3}},
	}
	fields := schema.InferFields(samples)
	byName := map[string]schema.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"tags", "meta", "st"} {
		if byName[name].Type != schema.JSON {
			t.Fatalf("field %q should infer json, got %s", name, byName[name].Type)
		}
	}
	if _, err := st.CreateTable(ctx, "test", "jsv", fields); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "jsv", samples, testEmbed); err != nil {
		t.Fatalf("inserting marshalable json values must work: %v", err)
	}
}
