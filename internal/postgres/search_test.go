package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func seedSearchTable(t *testing.T, s *Store) context.Context {
	t.Helper()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{
		{Name: "title", Fulltext: true},
		{Name: "body", Type: schema.Text, Fulltext: true},
		{Name: "kind"},
	}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	records := []map[string]any{
		{"title": "payment gateway", "body": "the gateway processes a payment refund", "kind": "doc"},
		{"title": "refund policy", "body": "payment terms and conditions", "kind": "policy"},
		{"title": "unrelated notice", "body": "nothing relevant here", "kind": "doc"},
		{"title": "payments", "body": "paying for things", "kind": "doc"},
	}
	if _, err := s.Insert(ctx, "app", "notes", records, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func searchIDs(t *testing.T, result store.SearchResult) []int64 {
	t.Helper()
	ids := make([]int64, len(result.Rows))
	for i, row := range result.Rows {
		id, ok := row["id"].(int64)
		if !ok {
			t.Fatalf("row %d has no int64 id: %+v", i, row)
		}
		ids[i] = id
	}
	return ids
}

func TestPostgresSearchFulltextMatchesAndRanks(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	result, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	ids := searchIDs(t, result)
	if len(ids) != 3 {
		t.Fatalf("payment matched %+v", ids)
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen[1] || !seen[2] || !seen[4] {
		t.Fatalf("stemmed match missed rows: %+v", ids)
	}
	if seen[3] {
		t.Fatalf("unrelated row matched: %+v", ids)
	}
	if result.Truncated {
		t.Fatal("unexpected truncation")
	}
	for _, row := range result.Rows {
		if _, ok := row["_embedding"]; ok {
			t.Fatalf("hidden column exposed: %+v", row)
		}
		if _, ok := row[ftsColumn]; ok {
			t.Fatalf("fts column exposed: %+v", row)
		}
		if row["title"] == nil || row["created_at"] == nil {
			t.Fatalf("row shape: %+v", row)
		}
	}
}

func TestPostgresSearchFulltextGrammar(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	for _, tc := range []struct {
		match string
		want  []int64
	}{
		{"payment gateway", []int64{1}},
		{"payment AND gateway", []int64{1}},
		{"gateway OR policy", []int64{1, 2}},
		{"payment NOT gateway", []int64{2, 4}},
		{`"payment refund"`, []int64{1}},
		{`"refund payment"`, nil},
		{`"processes a payment"`, []int64{1}},
		{"pay*", []int64{1, 2, 4}},
		{"nothing", []int64{3}},
	} {
		result, err := s.SearchFulltext(ctx, "app", "notes", tc.match, "", nil, false, nil, store.Incarnation{}, store.Page{})
		if err != nil {
			t.Fatalf("%q: %v", tc.match, err)
		}
		got := searchIDs(t, result)
		seen := map[int64]bool{}
		for _, id := range got {
			seen[id] = true
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%q matched %+v, want %+v", tc.match, got, tc.want)
		}
		for _, id := range tc.want {
			if !seen[id] {
				t.Fatalf("%q matched %+v, want %+v", tc.match, got, tc.want)
			}
		}
	}
}

func TestPostgresSearchFulltextFilterAndPaging(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	result, err := s.SearchFulltext(ctx, "app", "notes", "payment", "kind = ?", []any{"policy"}, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, result); len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("filtered search: %+v", ids)
	}
	first, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != 1 || !first.Truncated {
		t.Fatalf("first page: rows=%d truncated=%v", len(first.Rows), first.Truncated)
	}
	second, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Rows) != 1 {
		t.Fatalf("second page: %+v", second.Rows)
	}
	if searchIDs(t, first)[0] == searchIDs(t, second)[0] {
		t.Fatal("offset returned the same row")
	}
	last, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 10, Offset: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Rows) != 0 || last.Truncated {
		t.Fatalf("past the end: %+v truncated=%v", last.Rows, last.Truncated)
	}
	if _, err := s.SearchFulltext(ctx, "app", "notes", "payment", "kind = ? AND", []any{"doc"}, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("malformed filter: %v", err)
	}
	if _, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{Offset: -1}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("negative offset: %v", err)
	}
}

func TestPostgresSearchFulltextRequiresFulltextField(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "plain", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchFulltext(ctx, "app", "plain", "anything", "", nil, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("table without fulltext fields: %v", err)
	}
}

func TestPostgresSearchFulltextFollowsMigrations(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpSetFulltext, Name: "kind", Value: migrateTrue()},
	}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	result, err := s.SearchFulltext(ctx, "app", "notes", "policy", "", nil, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, result); len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("newly indexed field: %+v", ids)
	}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpDropField, Name: "title"},
	}, store.Embedder{}, store.Incarnation{Version: 2}); err != nil {
		t.Fatal(err)
	}
	result, err = s.SearchFulltext(ctx, "app", "notes", "gateway", "", nil, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, result); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("after dropping an indexed field body should still match: %+v", ids)
	}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpSetFulltext, Name: "body", Value: new(bool)},
		{Op: schema.OpSetFulltext, Name: "kind", Value: new(bool)},
	}, store.Embedder{}, store.Incarnation{Version: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchFulltext(ctx, "app", "notes", "gateway", "", nil, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("search after removing every fulltext field: %v", err)
	}
}

func vectorQuery(vec []float32) store.VectorQuery {
	return store.VectorQuery{Vec: vec}
}

func TestPostgresSearchVectorRanksAndScores(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "label"}, {Name: "vec", Type: schema.Vector, Dim: 3}}
	if _, err := s.CreateTable(ctx, "app", "points", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	records := []map[string]any{
		{"label": "east", "vec": []any{1.0, 0.0, 0.0}},
		{"label": "north", "vec": []any{0.0, 1.0, 0.0}},
		{"label": "diagonal", "vec": []any{1.0, 1.0, 0.0}},
		{"label": "missing"},
	}
	if _, err := s.Insert(ctx, "app", "points", records, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.SearchVector(ctx, "app", "points", vectorQuery([]float32{1, 0, 0}), false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	ids := searchIDs(t, result)
	if len(ids) != 3 || ids[0] != 1 {
		t.Fatalf("ranking: %+v", ids)
	}
	if ids[1] != 3 || ids[2] != 2 {
		t.Fatalf("cosine order: %+v", ids)
	}
	if result.Execution != store.VectorExact {
		t.Fatalf("execution: %q", result.Execution)
	}
	if result.SkippedVectors != 0 {
		t.Fatalf("skipped: %d", result.SkippedVectors)
	}
	top, ok := result.Rows[0]["_score"].(float64)
	if !ok || top < 0.999 {
		t.Fatalf("top score: %+v", result.Rows[0]["_score"])
	}
	for _, row := range result.Rows {
		if _, ok := row["_score"].(float64); !ok {
			t.Fatalf("row without score: %+v", row)
		}
	}
}

func TestPostgresSearchVectorMinScoreFilterAndPaging(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "kind"}, {Name: "vec", Type: schema.Vector, Dim: 3}}
	if _, err := s.CreateTable(ctx, "app", "points", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	records := []map[string]any{
		{"kind": "a", "vec": []any{1.0, 0.0, 0.0}},
		{"kind": "b", "vec": []any{1.0, 1.0, 0.0}},
		{"kind": "a", "vec": []any{0.0, 1.0, 0.0}},
	}
	if _, err := s.Insert(ctx, "app", "points", records, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	min := 0.9
	q := vectorQuery([]float32{1, 0, 0})
	q.MinScore = &min
	result, err := s.SearchVector(ctx, "app", "points", q, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, result); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("min_score: %+v", ids)
	}
	filtered := vectorQuery([]float32{1, 0, 0})
	filtered.Filter = "kind = ?"
	filtered.Args = []any{"a"}
	result, err = s.SearchVector(ctx, "app", "points", filtered, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, result); len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Fatalf("filtered: %+v", ids)
	}
	first, err := s.SearchVector(ctx, "app", "points", vectorQuery([]float32{1, 0, 0}), false, nil, store.Incarnation{}, store.Page{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != 1 || !first.Truncated {
		t.Fatalf("first page: rows=%d truncated=%v", len(first.Rows), first.Truncated)
	}
	second, err := s.SearchVector(ctx, "app", "points", vectorQuery([]float32{1, 0, 0}), false, nil, store.Incarnation{}, store.Page{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, second); len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("second page: %+v", ids)
	}
	bad := vectorQuery([]float32{1, 0})
	if _, err := s.SearchVector(ctx, "app", "points", bad, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("dimension mismatch: %v", err)
	}
}

func TestPostgresSearchVectorCountsSkippedVectors(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "vec", Type: schema.Vector, Dim: 3}}
	if _, err := s.CreateTable(ctx, "app", "points", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "points", []map[string]any{
		{"vec": []any{1.0, 0.0, 0.0}},
		{"vec": []any{0.0, 1.0, 0.0}},
	}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "points")
		if err != nil {
			return err
		}
		conn, err := pgx.Connect(ctx, cfg.DSN)
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		_, err = conn.Exec(ctx, "UPDATE "+ident(n.physical, state.physical)+" SET "+ident(state.columns["vec"])+" = $1 WHERE id = 2", []byte{1, 2, 3, 4, 5})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result, err := s.SearchVector(ctx, "app", "points", vectorQuery([]float32{1, 0, 0}), false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SkippedVectors != 1 {
		t.Fatalf("skipped = %d, want 1", result.SkippedVectors)
	}
	if ids := searchIDs(t, result); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("corrupt vector returned: %+v", ids)
	}
}

func TestPostgresSearchVectorUsesEmbeddingColumn(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Vectorize: true}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	emb := store.Embedder{Identity: "test", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			if text == "east" {
				out[i] = []float32{1, 0, 0}
				continue
			}
			out[i] = []float32{0, 1, 0}
		}
		return out, nil
	}}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "east"}, {"body": "north"}}, store.WriteOpts{}, emb, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.SearchVector(ctx, "app", "notes", vectorQuery([]float32{1, 0, 0}), false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, result); len(ids) != 2 || ids[0] != 1 {
		t.Fatalf("embedding search: %+v", ids)
	}
	if _, ok := result.Rows[0]["_embedding"]; ok {
		t.Fatalf("hidden column exposed: %+v", result.Rows[0])
	}
	hidden, err := s.SearchVector(ctx, "app", "notes", vectorQuery([]float32{1, 0, 0}), true, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	vec, ok := hidden.Rows[0]["_embedding"].([]float64)
	if !ok || len(vec) != 3 {
		t.Fatalf("include_hidden embedding: %+v", hidden.Rows[0]["_embedding"])
	}
}

func TestPostgresSearchIgnoresArgsWithoutFilter(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	result, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", []any{"unused"}, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatalf("fulltext args without a filter: %v", err)
	}
	if len(result.Rows) != 3 {
		t.Fatalf("fulltext rows: %+v", searchIDs(t, result))
	}
	if _, err := s.CreateTable(ctx, "app", "points", []schema.Field{{Name: "vec", Type: schema.Vector, Dim: 3}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "points", []map[string]any{{"vec": []any{1.0, 0.0, 0.0}}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	q := vectorQuery([]float32{1, 0, 0})
	q.Args = []any{"unused"}
	vectorResult, err := s.SearchVector(ctx, "app", "points", q, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatalf("vector args without a filter: %v", err)
	}
	if len(vectorResult.Rows) != 1 {
		t.Fatalf("vector rows: %+v", searchIDs(t, vectorResult))
	}
}

func TestPostgresFulltextIndexNameAvoidsExistingTable(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes_fts", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "title", Fulltext: true}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("fulltext table blocked by a sibling named notes_fts: %v", err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"title": "payment gateway"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("search: %+v", result.Rows)
	}
	if _, err := s.Insert(ctx, "app", "notes_fts", []map[string]any{{"body": "sibling still writable"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("sibling table damaged: %v", err)
	}
}

func TestPostgresMigrateFulltextIndexNameAvoidsExistingTable(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes_fts", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "title"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"title": "refund policy"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpSetFulltext, Name: "title", Value: migrateTrue()},
	}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil {
		t.Fatalf("set_fulltext blocked by a sibling named notes_fts: %v", err)
	}
	result, err := s.SearchFulltext(ctx, "app", "notes", "refund", "", nil, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("search after migration: %+v", result.Rows)
	}
}

func TestPostgresSearchFulltextRejectsMistranslatableOperators(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	for _, match := range []string{"^payment", "payment ^gateway", "payment + gateway", "payment+gateway"} {
		if _, err := s.SearchFulltext(ctx, "app", "notes", match, "", nil, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("%q was accepted rather than rejected: %v", match, err)
		}
	}
	if _, err := s.SearchFulltext(ctx, "app", "notes", `"payment gateway"`, "", nil, false, nil, store.Incarnation{}, store.Page{}); err != nil {
		t.Fatalf("the suggested phrase alternative was rejected: %v", err)
	}
}
