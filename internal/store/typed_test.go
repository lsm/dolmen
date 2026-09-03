package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func typedFields() []schema.Field {
	return []schema.Field{
		{Name: "s", Type: schema.String},
		{Name: "t", Type: schema.Text, Fulltext: true},
		{Name: "n", Type: schema.Number},
		{Name: "f", Type: schema.Number},
		{Name: "b", Type: schema.Boolean},
		{Name: "at", Type: schema.Timestamp},
		{Name: "j", Type: schema.JSON},
		{Name: "vec", Type: schema.Vector, Dim: 4},
	}
}

func mustCreateTyped(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.CreateTable(context.Background(), "test", "typed", typedFields()); err != nil {
		t.Fatalf("create typed table: %v", err)
	}
}

func mustInsertTyped(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.Insert(context.Background(), "test", "typed", []map[string]any{
		{"s": "hello", "t": "the needle rests here", "n": 42, "f": 2.5, "b": true,
			"at": "2026-09-01T10:00:00Z", "j": map[string]any{"k": []any{json.Number("1"), "x"}},
			"vec": []any{1.0, 2.0, 3.0, 4.0}},
		{"b": false},
	}, testEmbed); err != nil {
		t.Fatalf("insert typed records: %v", err)
	}
}

// assertTypedRow checks the typed-read contract for the full row: every
// declared field type comes back as its JSON-shaped Go value, not its SQL
// storage form.
func assertTypedRow(t *testing.T, row map[string]any) {
	t.Helper()
	if row["s"] != "hello" || row["t"] != "the needle rests here" {
		t.Fatalf("string/text fields must stay strings: %v", row)
	}
	if n, ok := row["n"].(int64); !ok || n != 42 {
		t.Fatalf("integer number must round-trip as int64, got %T %v", row["n"], row["n"])
	}
	if f, ok := row["f"].(float64); !ok || f != 2.5 {
		t.Fatalf("fractional number must round-trip as float64, got %T %v", row["f"], row["f"])
	}
	if b, ok := row["b"].(bool); !ok || !b {
		t.Fatalf("boolean must round-trip as bool, got %T %v", row["b"], row["b"])
	}
	if at, ok := row["at"].(string); !ok || at != "2026-09-01T10:00:00Z" {
		t.Fatalf("timestamp must round-trip as its stored string, got %T %v", row["at"], row["at"])
	}
	j, ok := row["j"].(map[string]any)
	if !ok {
		t.Fatalf("json field must decode to a map, got %T %v", row["j"], row["j"])
	}
	k, ok := j["k"].([]any)
	if !ok || len(k) != 2 || k[0] != json.Number("1") || k[1] != "x" {
		t.Fatalf("nested json must decode with number precision kept, got %v", j["k"])
	}
	vec, ok := row["vec"].([]float64)
	if !ok || len(vec) != 4 || vec[0] != 1 || vec[3] != 4 {
		t.Fatalf("vector must decode to []float64, got %T %v", row["vec"], row["vec"])
	}
}

func TestTypedReadRoundTripQuery(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateTyped(t, st)
	mustInsertTyped(t, st)

	rows, _, err := st.Query(ctx, "test", "SELECT * FROM typed ORDER BY id", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	assertTypedRow(t, rows[0])

	// The sparse row keeps SQL NULL as JSON null and false distinct from null.
	sparse := rows[1]
	if b, ok := sparse["b"].(bool); !ok || b {
		t.Fatalf("explicit false must come back as bool false, got %T %v", sparse["b"], sparse["b"])
	}
	for _, col := range []string{"s", "t", "n", "f", "at", "j", "vec"} {
		if sparse[col] != nil {
			t.Fatalf("unset field %s must read back as null, got %T %v", col, sparse[col], sparse[col])
		}
	}
	if _, ok := sparse["_embedding"]; ok {
		t.Fatal("query must not synthesize an _embedding column")
	}
}

func TestTypedReadRoundTripSearchFulltext(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateTyped(t, st)
	mustInsertTyped(t, st)

	rows, _, err := st.SearchFulltext(ctx, "test", "typed", "needle", 0, 10, false)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 fts hit, got %d", len(rows))
	}
	assertTypedRow(t, rows[0])
}

func TestTypedReadRoundTripSearchVector(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateTyped(t, st)
	mustInsertTyped(t, st)

	rows, _, err := st.SearchVector(ctx, "test", "typed", "vec", []float32{1, 0, 0, 0}, "", 0, 10, false)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 vector hit, got %d", len(rows))
	}
	assertTypedRow(t, rows[0])
	if _, ok := rows[0]["_score"].(float64); !ok {
		t.Fatalf("vector search must attach _score, got %T %v", rows[0]["_score"], rows[0]["_score"])
	}
}

func TestSearchHidesEmbeddingByDefault(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	qv, _ := fakeEmbed(ctx, []string{"the dolmen stores stone tables"})

	fts, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 0, 10, false)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(fts) != 1 {
		t.Fatalf("expected 1 fts hit, got %d", len(fts))
	}
	if _, has := fts[0]["_embedding"]; has {
		t.Fatal("search_fulltext must hide _embedding by default")
	}
	if vec, ok := fts[0]["emb"].([]float64); !ok || len(vec) != 4 {
		t.Fatalf("declared vector column must still come back typed, got %T %v", fts[0]["emb"], fts[0]["emb"])
	}

	vec, _, err := st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 0, 1, false)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(vec) != 1 || vec[0]["title"] != "first note" {
		t.Fatalf("unexpected vector hit: %v", vec)
	}
	if _, has := vec[0]["_embedding"]; has {
		t.Fatal("search_vector must hide _embedding by default")
	}

	fts, _, err = st.SearchFulltext(ctx, "test", "notes", "dolmen", 0, 10, true)
	if err != nil {
		t.Fatalf("fts include_hidden: %v", err)
	}
	emb, ok := fts[0]["_embedding"].([]float64)
	if !ok || len(emb) != 8 {
		t.Fatalf("include_hidden must return _embedding typed, got %T %v", fts[0]["_embedding"], fts[0]["_embedding"])
	}

	vec, _, err = st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 0, 1, true)
	if err != nil {
		t.Fatalf("vector search include_hidden: %v", err)
	}
	if emb, ok := vec[0]["_embedding"].([]float64); !ok || len(emb) != 8 {
		t.Fatalf("include_hidden must return _embedding typed, got %T %v", vec[0]["_embedding"], vec[0]["_embedding"])
	}
}

func TestQueryEmbeddingProjection(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	rows, _, err := st.Query(ctx, "test", "SELECT * FROM notes ORDER BY id", nil, 0, 0)
	if err != nil {
		t.Fatalf("query star: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i, row := range rows {
		if _, has := row["_embedding"]; has {
			t.Fatalf("row %d: SELECT * must not leak _embedding", i)
		}
	}
	if b, ok := rows[0]["done"].(bool); !ok || !b {
		t.Fatalf("boolean must be typed in SELECT *, got %T %v", rows[0]["done"], rows[0]["done"])
	}

	rows, _, err = st.Query(ctx, "test", "SELECT id, _embedding FROM notes WHERE title = 'first note'", nil, 0, 0)
	if err != nil {
		t.Fatalf("query explicit embedding: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	emb, ok := rows[0]["_embedding"].([]float64)
	if !ok || len(emb) != 8 {
		t.Fatalf("explicitly selected _embedding must come back typed, got %T %v", rows[0]["_embedding"], rows[0]["_embedding"])
	}
}

func TestQueryAmbiguousColumnTypeNotCoerced(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "amb_a", []schema.Field{
		{Name: "x", Type: schema.Boolean},
		{Name: "y", Type: schema.Boolean},
	}); err != nil {
		t.Fatalf("create amb_a: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "amb_b", []schema.Field{
		{Name: "x", Type: schema.String},
	}); err != nil {
		t.Fatalf("create amb_b: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "amb_a", []map[string]any{{"x": true, "y": true}}, testEmbed); err != nil {
		t.Fatalf("insert amb_a: %v", err)
	}

	rows, _, err := st.Query(ctx, "test", "SELECT x, y FROM amb_a", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if raw, ok := rows[0]["x"].(int64); !ok || raw != 1 {
		t.Fatalf("a field name declared with conflicting types across tables must stay raw, got %T %v", rows[0]["x"], rows[0]["x"])
	}
	if b, ok := rows[0]["y"].(bool); !ok || !b {
		t.Fatalf("an unambiguous field in the same namespace must still coerce, got %T %v", rows[0]["y"], rows[0]["y"])
	}
}

func TestQueryExpressionColumnsFallBack(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateTyped(t, st)
	mustInsertTyped(t, st)

	rows, _, err := st.Query(ctx, "test",
		"SELECT zeroblob(12) AS rawblob, count(*) AS c, 1.5 AS f FROM typed", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	blob, ok := rows[0]["rawblob"].(string)
	if !ok || len(blob) != 16 { // base64 of 12 zero bytes
		t.Fatalf("expression blobs must fall back to base64, got %T %v", rows[0]["rawblob"], rows[0]["rawblob"])
	}
	if c, ok := rows[0]["c"].(int64); !ok || c != 2 {
		t.Fatalf("count(*) must stay int64, got %T %v", rows[0]["c"], rows[0]["c"])
	}
	if f, ok := rows[0]["f"].(float64); !ok || f != 1.5 {
		t.Fatalf("literal float must stay float64, got %T %v", rows[0]["f"], rows[0]["f"])
	}
}

func TestJSONNumberPrecisionRoundTrip(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateTyped(t, st)
	if _, err := st.Insert(ctx, "test", "typed", []map[string]any{
		{"j": map[string]any{"big": json.Number("9007199254740993")}},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT j FROM typed", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	big, ok := rows[0]["j"].(map[string]any)["big"].(json.Number)
	if !ok || big.String() != "9007199254740993" {
		t.Fatalf("json integers must keep precision, got %T %v", rows[0]["j"], rows[0]["j"])
	}
}

func TestMentionsEmbedding(t *testing.T) {
	cases := []struct {
		stmt string
		want bool
	}{
		{"SELECT _embedding FROM notes", true},
		{"select id, _EMBEDDING from notes", true},
		{`SELECT * FROM notes WHERE "_embedding" IS NOT NULL`, true},
		{"SELECT * FROM notes WHERE title = '_embedding'", false},
		{"SELECT 'a''_embedding''b' AS x FROM notes", false},
		{"SELECT * FROM notes -- _embedding", false},
		{"SELECT * /* _embedding */ FROM notes", false},
		{"SELECT * FROM notes WHERE title = 'x' OR body = '_embedding'", false},
		{"SELECT 1 AS x_embedding FROM notes", false},
	}
	for _, tc := range cases {
		if got := mentionsEmbedding(tc.stmt); got != tc.want {
			t.Errorf("mentionsEmbedding(%q) = %v, want %v", tc.stmt, got, tc.want)
		}
	}
}

func TestQueryEmbeddingLiteralDoesNotOptIn(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "_embedding", "body": "a row literally named after the hidden column"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT * FROM notes WHERE title = '_embedding'", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the literal-titled row, got %d rows", len(rows))
	}
	if _, has := rows[0]["_embedding"]; has {
		t.Fatal("a _embedding string literal must not opt the hidden column in")
	}
}

func TestQueryAliasToDeclaredNameCoercesByLabel(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateTyped(t, st)
	mustInsertTyped(t, st)

	// Coercion is by result-column label: an expression aliased to a declared
	// boolean field name takes that field's presentation, while values outside
	// the boolean storage shape (0/1) stay raw.
	rows, _, err := st.Query(ctx, "test", "SELECT 1 AS b, 2 AS b2 FROM typed", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["b"] != true {
		t.Fatalf("label b is declared boolean, so 1 must present as true, got %T %v", rows[0]["b"], rows[0]["b"])
	}
	if raw, ok := rows[0]["b2"].(int64); !ok || raw != 2 {
		t.Fatalf("non-0/1 values must stay raw even under a boolean label, got %T %v", rows[0]["b2"], rows[0]["b2"])
	}
}
