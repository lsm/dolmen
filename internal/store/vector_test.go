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

	res, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 2, false, "", nil, nil)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res.Rows) != 2 || res.Rows[0]["title"] != "first note" {
		t.Fatalf("unexpected vector results: %v", res.Rows)
	}
	if score := res.Rows[0]["_score"].(float64); score < 0.99 {
		t.Fatalf("expected cosine ~1, got %f", score)
	}
	if res.Skipped != 0 {
		t.Fatalf("expected no skipped rows, got %d", res.Skipped)
	}

	if _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0}, "", 2, false, "", nil, nil); err == nil {
		t.Fatal("expected dim mismatch to be rejected")
	}

	qv, _ := fakeEmbed(ctx, []string{"the dolmen stores stone tables"})
	res, err = st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 1, false, "", nil, nil)
	if err != nil {
		t.Fatalf("vectorize search: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["title"] != "first note" {
		t.Fatalf("unexpected vectorize results: %v", res.Rows)
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
	if _, err := st.SearchVector(ctx, "test", "auto", "", []float32{1, 0, 0, 0}, "", 5, false, "", nil, nil); err == nil {
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
	res, err := st.SearchVector(ctx, "test", "vecbud", "emb", make([]float32, 4096), "", 200, false, "", nil, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !res.Truncated || len(res.Rows) >= 150 {
		t.Fatalf("decorated vectors must count against the budget: %d rows truncated=%v", len(res.Rows), res.Truncated)
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
	res, err := st.SearchVector(ctx, "test", "vlim", "emb", []float32{1, 0}, "", -1, false, "", nil, nil)
	if err != nil {
		t.Fatalf("search with negative limit must be bounded, not error: %v", err)
	}
	if len(res.Rows) > 200 {
		t.Fatalf("negative limit must clamp to 200, got %d rows", len(res.Rows))
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
		if _, err := st.SearchVector(ctx, "test", "nfq", "emb", bad, "", 5, false, "", nil, nil); err == nil {
			t.Fatalf("expected non-finite query vector to be rejected: %v", bad)
		}
	}
}

func TestTextQueryCannotTargetRawVectorColumn(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	// rawvec has no vectorize field, only a caller-provided vector column:
	// a text query has no defensible space to compare against.
	if _, err := st.CreateTable(ctx, "test", "rawvec", []schema.Field{
		{Name: "s", Type: schema.String},
		{Name: "emb", Type: schema.Vector, Dim: 4},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "rawvec", []map[string]any{
		{"s": "hello", "emb": []any{1.0, 0, 0, 0}},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	qv, _ := fakeEmbed(ctx, []string{"hello"})
	if _, err := st.SearchVector(ctx, "test", "rawvec", "", qv[0], "fake-space", 5, false, "", nil, nil); err == nil {
		t.Fatal("text query without column must not silently fall back to a caller-provided vector column")
	} else if !errors.Is(err, ErrInvalid) {
		t.Fatalf("text query without column: expected ErrInvalid, got %v", err)
	}
	if _, err := st.SearchVector(ctx, "test", "rawvec", "emb", qv[0], "fake-space", 5, false, "", nil, nil); err == nil {
		t.Fatal("text query naming a caller-provided vector column must be rejected")
	} else if !errors.Is(err, ErrInvalid) {
		t.Fatalf("text query naming a vector column: expected ErrInvalid, got %v", err)
	}
	// The same table stays searchable with raw vectors, with and without column:
	// the caller owns matching the embedding space.
	for _, column := range []string{"", "emb"} {
		res, err := st.SearchVector(ctx, "test", "rawvec", column, []float32{1, 0, 0, 0}, "", 5, false, "", nil, nil)
		if err != nil {
			t.Fatalf("raw-vector search with column %q: %v", column, err)
		}
		if len(res.Rows) != 1 || res.Rows[0]["s"] != "hello" {
			t.Fatalf("unexpected raw-vector results with column %q: %v", column, res.Rows)
		}
	}
	// The fail-fast validation used before spending an embedding rejects too.
	for _, column := range []string{"", "emb"} {
		if err := st.ValidateVectorSearch(ctx, "test", "rawvec", column, "fake-space"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidateVectorSearch with column %q: expected ErrInvalid, got %v", column, err)
		}
	}
	// A vectorized table also rejects naming its raw vector column for a text
	// query, even though its own _embedding space would be fine.
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)
	if _, err := st.SearchVector(ctx, "test", "notes", "emb", qv[0], "fake-space", 5, false, "", nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("text query naming a raw vector column on a vectorized table: expected ErrInvalid, got %v", err)
	}
}

func TestTextQueryOnTableWithoutAnyVectorData(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "plain", []schema.Field{
		{Name: "s", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	qv, _ := fakeEmbed(ctx, []string{"hello"})
	if _, err := st.SearchVector(ctx, "test", "plain", "", qv[0], "fake-space", 5, false, "", nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for text query on a table with no vector data, got %v", err)
	}
}

func TestSearchVectorSurfacesSkippedCorruptVectors(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "vsk", []schema.Field{
		{Name: "s", Type: schema.String},
		{Name: "emb", Type: schema.Vector, Dim: 3},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "vsk", []map[string]any{
		{"s": "good", "emb": []any{1.0, 0, 0}},
		{"s": "truncated blob", "emb": []any{1.0, 0, 0}},
		{"s": "non-finite", "emb": []any{1.0, 0, 0}},
		{"s": "wrong dim", "emb": []any{1.0, 0, 0}},
		{"s": "integer", "emb": []any{1.0, 0, 0}},
		{"s": "real", "emb": []any{1.0, 0, 0}},
		{"s": "text", "emb": []any{1.0, 0, 0}},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Corrupt rows the way only an out-of-band SQLite writer could: the store
	// itself never writes these, so search must report them, not hide them —
	// in any storage type, not just malformed BLOBs.
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	for _, upd := range []struct {
		where string
		sql   string
		args  []any
	}{
		{"truncated blob", `UPDATE vsk SET emb = X'010203' WHERE s = ?`, nil},
		{"non-finite", `UPDATE vsk SET emb = ? WHERE s = ?`, []any{schema.EncodeVector([]float32{1, float32(math.NaN()), 0})}},
		{"wrong dim", `UPDATE vsk SET emb = ? WHERE s = ?`, []any{schema.EncodeVector([]float32{1, 2, 3, 4})}},
		{"integer", `UPDATE vsk SET emb = 7 WHERE s = ?`, nil},
		{"real", `UPDATE vsk SET emb = 2.5 WHERE s = ?`, nil},
		{"text", `UPDATE vsk SET emb = 'abc' WHERE s = ?`, nil},
	} {
		args := upd.args
		if args == nil {
			args = []any{upd.where}
		} else {
			args = append(args, upd.where)
		}
		if _, err := n.rw.ExecContext(ctx, upd.sql, args...); err != nil {
			t.Fatalf("corrupt %q: %v", upd.where, err)
		}
	}

	res, err := st.SearchVector(ctx, "test", "vsk", "emb", []float32{1, 0, 0}, "", 10, false, "", nil, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["s"] != "good" {
		t.Fatalf("expected only the intact row, got %v", res.Rows)
	}
	if res.Skipped != 6 {
		t.Fatalf("expected 6 skipped rows (truncated blob, non-finite, wrong dim, integer, real, text), got %d", res.Skipped)
	}
}

func TestSearchVectorFilter(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	// Filter to rows whose title matches a bound parameter.
	res, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "title = ?", []any{"first note"}, nil)
	if err != nil {
		t.Fatalf("filtered vector search: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["title"] != "first note" {
		t.Fatalf("expected one filtered hit, got %v", res.Rows)
	}

	// Filter on a numeric metadata column.
	res, err = st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "score >= 3", nil, nil)
	if err != nil {
		t.Fatalf("numeric filter search: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 hits with score >= 3, got %v", res.Rows)
	}

	// Filter with no matches returns empty, not an error.
	res, err = st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "title = ?", []any{"missing"}, nil)
	if err != nil {
		t.Fatalf("zero-match filter search: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("expected 0 hits for missing filter, got %v", res.Rows)
	}

	// Malicious/invalid filters are rejected like delete's filter.
	if _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "1=1; DROP TABLE notes", nil, nil); err == nil {
		t.Fatal("expected semicolon in filter to be rejected")
	}
	if _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 10, false, "title = ? AND score > ?", []any{"first note"}, nil); err == nil {
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
	res, err := st.SearchVector(ctx, "test", "notes", "emb", q, "", 10, false, "", nil, nil)
	if err != nil {
		t.Fatalf("unfiltered search: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 hits without threshold, got %v", res.Rows)
	}

	// Threshold just below the top score: only first and third notes.
	cut := 0.95
	res, err = st.SearchVector(ctx, "test", "notes", "emb", q, "", 10, false, "", nil, &cut)
	if err != nil {
		t.Fatalf("min_score search: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 hits with min_score 0.95, got %v", res.Rows)
	}
	for _, r := range res.Rows {
		if r["_score"].(float64) < cut {
			t.Fatalf("result score %v below min_score %v", r["_score"], cut)
		}
	}

	// Threshold above the top score: empty.
	high := 1.01
	res, err = st.SearchVector(ctx, "test", "notes", "emb", q, "", 10, false, "", nil, &high)
	if err != nil {
		t.Fatalf("high min_score search: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("expected 0 hits above perfect score, got %v", res.Rows)
	}

	// Threshold and filter together.
	cut = 0.95
	res, err = st.SearchVector(ctx, "test", "notes", "emb", q, "", 10, false, "score >= 3", nil, &cut)
	if err != nil {
		t.Fatalf("filter + min_score search: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["title"] != "first note" {
		t.Fatalf("expected first note only, got %v", res.Rows)
	}
}
