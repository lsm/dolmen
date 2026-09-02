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

type blankIdentityEmb struct{}

func (blankIdentityEmb) Name() string     { return "blank" }
func (blankIdentityEmb) Identity() string { return "" }
func (blankIdentityEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, 8)
	}
	return out, nil
}

type zeroVecEmb struct{}

func (zeroVecEmb) Name() string     { return "zero" }
func (zeroVecEmb) Identity() string { return "zero-space" }
func (zeroVecEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return [][]float32{{}}, nil
}

func vectorSearchWithProvider(t *testing.T, p interface {
	Name() string
	Identity() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}) int {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, p).Handler())
	t.Cleanup(srv.Close)
	code, _ := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "p",
		"table":     "t",
		"fields":    []map[string]any{{"name": "s", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatal("create failed")
	}
	code, _ = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "p", "table": "t", "text": "anything",
	})
	return code
}

func TestSearchVectorBlankIdentityRejected(t *testing.T) {
	if code := vectorSearchWithProvider(t, blankIdentityEmb{}); code != 400 {
		t.Fatalf("text search with a blank-identity provider must 400, got %d", code)
	}
}

func TestSearchVectorZeroDimEmbedRejected(t *testing.T) {
	if code := vectorSearchWithProvider(t, zeroVecEmb{}); code != 400 {
		t.Fatalf("zero-dimensional query embedding must 400, got %d", code)
	}
}

func TestMigrateSchemaParity(t *testing.T) {
	def, ok := Ops["migrate"]
	if !ok {
		t.Fatal("migrate op missing")
	}
	props := def.InputSchema["properties"].(map[string]any)
	changes := props["changes"].(map[string]any)
	if changes["minItems"] != 1 {
		t.Fatalf("changes must declare minItems 1, got %v", changes)
	}
	items := changes["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Fatalf("change items must reject unknown properties, got %v", items)
	}
	field := items["properties"].(map[string]any)["field"].(map[string]any)
	if field["additionalProperties"] != false {
		t.Fatalf("add_field definition must reuse the closed field schema, got %v", field)
	}
	if _, ok := field["properties"].(map[string]any)["name"]; !ok {
		t.Fatal("add_field definition must declare the required name property")
	}
	fts, ok := Ops["search_fulltext"]
	if !ok {
		t.Fatal("search_fulltext op missing")
	}
	query := fts.InputSchema["properties"].(map[string]any)["query"].(map[string]any)
	if query["minLength"] != 1 {
		t.Fatalf("fulltext query must declare minLength 1, got %v", query)
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

func TestQueryAndDeleteSchemaParity(t *testing.T) {
	q, ok := Ops["query"]
	if !ok {
		t.Fatal("query op missing")
	}
	props := q.InputSchema["properties"].(map[string]any)
	sqlP := props["sql"].(map[string]any)
	if sqlP["minLength"] != 1 {
		t.Fatalf("sql must declare minLength 1, got %v", sqlP)
	}
	if sqlP["pattern"] != `(?i)^\s*(select|with)\b` {
		t.Fatalf("sql must declare the read-only prefix pattern, got %v", sqlP["pattern"])
	}
	if props["args"].(map[string]any)["maxItems"] != 100 {
		t.Fatalf("args must declare maxItems 100, got %v", props["args"])
	}
	d, ok := Ops["delete"]
	if !ok {
		t.Fatal("delete op missing")
	}
	filter := d.InputSchema["properties"].(map[string]any)["filter"].(map[string]any)
	if filter["pattern"] != `\S` {
		t.Fatalf("filter must require a non-whitespace character, got %v", filter)
	}
	for _, name := range []string{"search_fulltext", "search_vector"} {
		def, ok := Ops[name]
		if !ok {
			t.Fatalf("%s op missing", name)
		}
		limitP := def.InputSchema["properties"].(map[string]any)["limit"].(map[string]any)
		if limitP["minimum"] != 1 || limitP["maximum"] != 200 {
			t.Fatalf("%s limit must declare 1..200 (no silent clamping), got %v", name, limitP)
		}
	}
	m, ok := Ops["migrate"]
	if !ok {
		t.Fatal("migrate op missing")
	}
	items := m.InputSchema["properties"].(map[string]any)["changes"].(map[string]any)["items"].(map[string]any)
	mp := items["properties"].(map[string]any)
	for _, key := range []string{"from", "to", "name"} {
		p := mp[key].(map[string]any)
		if p["pattern"] != `^[a-z][a-z0-9_]{0,63}$` {
			t.Fatalf("migrate %s must carry the field-name pattern, got %v", key, p)
		}
		if _, ok := p["not"].(map[string]any)["enum"]; !ok {
			t.Fatalf("migrate %s must exclude reserved identifiers, got %v", key, p)
		}
	}
}
