package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestEndToEndHTTP(t *testing.T) {
	srv := newTestServer(t)

	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"fields": []map[string]any{
			{"name": "title", "type": "string", "fulltext": true},
			{"name": "detail", "type": "text", "vectorize": true},
			{"name": "confidence", "type": "number"},
		},
	})
	if code != 200 || res["ok"] != true {
		t.Fatalf("create_table failed: %d %v", code, res)
	}

	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"records": []map[string]any{
			{"title": "auth bug", "detail": "token expiry not checked in middleware", "confidence": 0.9},
			{"title": "slow query", "detail": "missing index on users.email", "confidence": 0.7},
		},
	})
	if code != 200 || res["ok"] != true {
		t.Fatalf("insert failed: %d %v", code, res)
	}

	code, res = post(t, srv.URL, "search_fulltext", map[string]any{
		"namespace": "skills", "table": "findings", "query": "auth",
	})
	if code != 200 {
		t.Fatalf("search_fulltext failed: %d %v", code, res)
	}
	results := res["data"].(map[string]any)["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["title"] != "auth bug" {
		t.Fatalf("unexpected fts results: %v", results)
	}

	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "skills", "table": "findings", "text": "token expiry not checked in middleware",
	})
	if code != 200 {
		t.Fatalf("search_vector failed: %d %v", code, res)
	}
	results = res["data"].(map[string]any)["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("expected 2 vector results, got %v", results)
	}
	top := results[0].(map[string]any)
	if top["title"] != "auth bug" || top["_score"].(float64) < 0.99 {
		t.Fatalf("unexpected top vector hit: %v", top)
	}

	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "skills", "sql": "SELECT count(*) AS n FROM findings",
	})
	if code != 200 {
		t.Fatalf("query failed: %d %v", code, res)
	}
	rows := res["data"].(map[string]any)["rows"].([]any)
	if rows[0].(map[string]any)["n"].(float64) != 2 {
		t.Fatalf("unexpected count: %v", rows)
	}

	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace": "skills", "table": "findings",
		"records": []map[string]any{{"bogus": 1}},
	})
	if code != 400 {
		t.Fatalf("expected 400 for unknown field, got %d %v", code, res)
	}

	code, _ = post(t, srv.URL, "explode", map[string]any{})
	if code != 404 {
		t.Fatalf("expected 404 for unknown op, got %d", code)
	}
}

func TestMigrateSetValueRequiredOverHTTP(t *testing.T) {
	srv := newTestServer(t)
	code, _ := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "m",
		"table":     "t",
		"fields":    []map[string]any{{"name": "detail", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatal("create failed")
	}
	code, res := post(t, srv.URL, "migrate", map[string]any{
		"namespace": "m",
		"table":     "t",
		"changes":   []map[string]any{{"op": "set_vectorize", "name": "detail"}},
	})
	if code != 400 {
		t.Fatalf("omitted set_vectorize value must 400 (it would silently disable and clear embeddings), got %d %v", code, res)
	}
	code, res = post(t, srv.URL, "migrate", map[string]any{
		"namespace": "m",
		"table":     "t",
		"changes":   []map[string]any{{"op": "set_vectorize", "name": "detail", "value": true}},
	})
	if code != 200 {
		t.Fatalf("explicit value must pass, got %d %v", code, res)
	}
}

func TestSearchVectorBothFormsRejected(t *testing.T) {
	srv := newTestServer(t)
	code, res := post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "x",
		"table":     "t",
		"text":      "hello",
		"vector":    []float64{1, 0, 0, 0},
	})
	if code != 400 {
		t.Fatalf("supplying both text and vector must 400 (the vector would be silently ignored), got %d %v", code, res)
	}
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "x",
		"table":     "t",
	})
	if code != 400 {
		t.Fatalf("supplying neither text nor vector must 400, got %d %v", code, res)
	}
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "x",
		"table":     "t",
		"text":      "",
	})
	if code != 400 {
		t.Fatalf("empty text must 400, got %d %v", code, res)
	}
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "x",
		"table":     "t",
		"vector":    []float64{},
	})
	if code != 400 {
		t.Fatalf("empty vector must 400, got %d %v", code, res)
	}
	def, ok := Ops["search_vector"]
	if !ok {
		t.Fatal("search_vector op missing")
	}
	oneOf, ok := def.InputSchema["oneOf"].([]any)
	if !ok || len(oneOf) != 2 {
		t.Fatalf("schema must require exactly one query form via oneOf, got %v", def.InputSchema["oneOf"])
	}
}

func TestSearchVectorRawVectorIgnoresProviderIdentity(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	spaceA := store.Embedder{
		Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range out {
				out[i] = []float32{1, 0, 0, 0, 0, 0, 0, 0}
			}
			return out, nil
		},
		Identity: "space-a",
	}
	if _, err := st.CreateTable(context.Background(), "mix", "t", []schema.Field{
		{Name: "s", Type: schema.Text, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(context.Background(), "mix", "t", []map[string]any{{"s": "hello"}}, spaceA); err != nil {
		t.Fatalf("insert: %v", err)
	}
	srv := httptest.NewServer(New(st, fakeEmb{}).Handler())
	t.Cleanup(srv.Close)
	code, res := post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "mix",
		"table":     "t",
		"vector":    []float64{1, 0, 0, 0, 0, 0, 0, 0},
	})
	if code != 200 {
		t.Fatalf("raw-vector search must not be bound to the active provider identity (table is space-a, server is fake-space), got %d %v", code, res)
	}
}

type multiEmb struct{}

func (multiEmb) Name() string     { return "multi" }
func (multiEmb) Identity() string { return "multi-space" }
func (multiEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, 2), nil
}

func TestSearchVectorMultiEmbedResultRejected(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, multiEmb{}).Handler())
	t.Cleanup(srv.Close)
	code, _ := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "mt",
		"table":     "t",
		"fields":    []map[string]any{{"name": "s", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatal("create failed")
	}
	code, res := post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "mt", "table": "t", "text": "anything",
	})
	if code != 400 {
		t.Fatalf("multi-vector provider result must 400, got %d %v", code, res)
	}
}

func TestInsertBatchBoundsDeclared(t *testing.T) {
	def, ok := Ops["insert"]
	if !ok {
		t.Fatal("insert op missing")
	}
	records := def.InputSchema["properties"].(map[string]any)["records"].(map[string]any)
	if records["minItems"] != 1 || records["maxItems"] != store.MaxRecordsPerInsert {
		t.Fatalf("records must declare the store batch bounds 1..%d, got %v", store.MaxRecordsPerInsert, records)
	}
}

type emptyEmb struct{}

func (emptyEmb) Name() string     { return "empty" }
func (emptyEmb) Identity() string { return "empty-space" }
func (emptyEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return [][]float32{}, nil
}

func TestSearchVectorEmptyEmbedResultRejected(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, emptyEmb{}).Handler())
	t.Cleanup(srv.Close)
	code, _ := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "e",
		"table":     "t",
		"fields":    []map[string]any{{"name": "s", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatal("create failed")
	}
	code, res := post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "e", "table": "t", "text": "anything",
	})
	if code != 400 {
		t.Fatalf("empty provider result must 400, not panic the handler, got %d %v", code, res)
	}
}
