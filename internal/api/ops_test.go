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
	if sqlP["pattern"] != `^\s*([sS][eE][lL][eE][cC][tT]|[wW][iI][tT][hH])\b[^;]*;*\s*$` {
		t.Fatalf("sql must declare the read-only prefix with a pure trailing-semicolon suffix, got %v", sqlP["pattern"])
	}
	if props["args"].(map[string]any)["maxItems"] != 100 {
		t.Fatalf("args must declare maxItems 100, got %v", props["args"])
	}
	limitP := props["limit"].(map[string]any)
	if limitP["minimum"] != 1 || limitP["maximum"] != 1000 {
		t.Fatalf("query limit must declare 1..1000, got %v", limitP)
	}
	offsetP := props["offset"].(map[string]any)
	if offsetP["minimum"] != 0 {
		t.Fatalf("query offset must declare minimum 0, got %v", offsetP)
	}
	d, ok := Ops["delete"]
	if !ok {
		t.Fatal("delete op missing")
	}
	filter := d.InputSchema["properties"].(map[string]any)["filter"].(map[string]any)
	if filter["pattern"] != `\S` {
		t.Fatalf("filter must require a non-whitespace character, got %v", filter)
	}
	if _, ok := filter["not"].(map[string]any)["pattern"]; !ok {
		t.Fatalf("filter must exclude all semicolons, got %v", filter)
	}
	sv, ok := Ops["search_vector"]
	if !ok {
		t.Fatal("search_vector op missing")
	}
	vecItems := sv.InputSchema["properties"].(map[string]any)["vector"].(map[string]any)["items"].(map[string]any)
	if vecItems["maximum"].(float64) != 3.4028234663852886e+38 || vecItems["minimum"].(float64) != -3.4028234663852886e+38 {
		t.Fatalf("vector items must declare the float32 range, got %v", vecItems)
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
		offsetP := def.InputSchema["properties"].(map[string]any)["offset"].(map[string]any)
		if offsetP["minimum"] != 0 {
			t.Fatalf("%s offset must declare minimum 0, got %v", name, offsetP)
		}
	}
	m, ok := Ops["migrate"]
	if !ok {
		t.Fatal("migrate op missing")
	}
	items := m.InputSchema["properties"].(map[string]any)["changes"].(map[string]any)["items"].(map[string]any)
	allOf := items["allOf"].([]any)
	rankGuard, ok := allOf[len(allOf)-1].(map[string]any)["then"].(map[string]any)["properties"].(map[string]any)["name"].(map[string]any)
	if !ok || rankGuard["not"].(map[string]any)["const"] != "rank" {
		t.Fatalf("set_fulltext true must exclude the FTS5-reserved name rank, got %v", allOf[len(allOf)-1])
	}
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

// assertTypedHTTPRow checks the typed wire contract on one decoded result row:
// booleans as JSON booleans, json fields as decoded values, vectors as number
// arrays, and no hidden _embedding unless it was opted in.
func assertTypedHTTPRow(t *testing.T, row map[string]any, wantEmbedding bool) {
	t.Helper()
	if row["title"] != "typed reads" || row["at"] != "2026-09-01T10:00:00Z" {
		t.Fatalf("string/timestamp fields must stay strings: %v", row)
	}
	if row["done"] != true {
		t.Fatalf("boolean must arrive as a JSON boolean, got %T %v", row["done"], row["done"])
	}
	if score, ok := row["score"].(float64); !ok || score != 42 {
		t.Fatalf("number must arrive as a JSON number, got %T %v", row["score"], row["score"])
	}
	meta, ok := row["meta"].(map[string]any)
	if !ok {
		t.Fatalf("json field must arrive decoded, got %T %v", row["meta"], row["meta"])
	}
	if k, ok := meta["k"].([]any); !ok || len(k) != 2 || k[0] != float64(1) || k[1] != "x" {
		t.Fatalf("nested json must arrive decoded, got %v", meta["k"])
	}
	vec, ok := row["vec"].([]any)
	if !ok || len(vec) != 4 || vec[0] != float64(1) || vec[3] != float64(4) {
		t.Fatalf("vector must arrive as a number array, got %T %v", row["vec"], row["vec"])
	}
	_, has := row["_embedding"]
	if has != wantEmbedding {
		t.Fatalf("_embedding presence must be %v, got %v", wantEmbedding, has)
	}
	if !wantEmbedding {
		return
	}
	emb, ok := row["_embedding"].([]any)
	if !ok || len(emb) != 8 {
		t.Fatalf("_embedding must arrive as a typed array when included, got %T %v", row["_embedding"], row["_embedding"])
	}
}

func TestTypedReadContractHTTP(t *testing.T) {
	srv := newTestServer(t)

	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "typed",
		"table":     "docs",
		"fields": []map[string]any{
			{"name": "title", "type": "string", "fulltext": true},
			{"name": "body", "type": "text", "vectorize": true},
			{"name": "score", "type": "number"},
			{"name": "done", "type": "boolean"},
			{"name": "at", "type": "timestamp"},
			{"name": "meta", "type": "json"},
			{"name": "vec", "type": "vector", "dim": 4},
		},
	})
	if code != 200 {
		t.Fatalf("create_table failed: %d %v", code, res)
	}

	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace": "typed",
		"table":     "docs",
		"records": []map[string]any{{
			"title": "typed reads", "body": "the contract holds", "score": 42, "done": true,
			"at": "2026-09-01T10:00:00Z", "meta": map[string]any{"k": []any{1, "x"}},
			"vec": []any{1.0, 2.0, 3.0, 4.0},
		}},
	})
	if code != 200 {
		t.Fatalf("insert failed: %d %v", code, res)
	}

	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "typed", "sql": "SELECT * FROM docs",
	})
	if code != 200 {
		t.Fatalf("query failed: %d %v", code, res)
	}
	rows := res["data"].(map[string]any)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %v", rows)
	}
	assertTypedHTTPRow(t, rows[0].(map[string]any), false)

	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "typed", "sql": "SELECT id, _embedding FROM docs",
	})
	if code != 200 {
		t.Fatalf("query with explicit _embedding failed: %d %v", code, res)
	}
	rows = res["data"].(map[string]any)["rows"].([]any)
	if emb, ok := rows[0].(map[string]any)["_embedding"].([]any); !ok || len(emb) != 8 {
		t.Fatalf("explicit _embedding must arrive typed, got %v", rows[0])
	}

	code, res = post(t, srv.URL, "search_fulltext", map[string]any{
		"namespace": "typed", "table": "docs", "query": "typed",
	})
	if code != 200 {
		t.Fatalf("search_fulltext failed: %d %v", code, res)
	}
	results := res["data"].(map[string]any)["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 fts hit, got %v", results)
	}
	assertTypedHTTPRow(t, results[0].(map[string]any), false)

	code, res = post(t, srv.URL, "search_fulltext", map[string]any{
		"namespace": "typed", "table": "docs", "query": "typed", "include_hidden": true,
	})
	if code != 200 {
		t.Fatalf("search_fulltext include_hidden failed: %d %v", code, res)
	}
	results = res["data"].(map[string]any)["results"].([]any)
	assertTypedHTTPRow(t, results[0].(map[string]any), true)

	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "typed", "table": "docs", "vector": []float64{1, 0, 0, 0}, "column": "vec",
	})
	if code != 200 {
		t.Fatalf("search_vector failed: %d %v", code, res)
	}
	results = res["data"].(map[string]any)["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 vector hit, got %v", results)
	}
	hit := results[0].(map[string]any)
	assertTypedHTTPRow(t, hit, false)
	if _, ok := hit["_score"].(float64); !ok {
		t.Fatalf("search_vector must attach _score, got %T %v", hit["_score"], hit["_score"])
	}

	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "typed", "table": "docs", "text": "the contract holds", "include_hidden": true,
	})
	if code != 200 {
		t.Fatalf("search_vector text include_hidden failed: %d %v", code, res)
	}
	results = res["data"].(map[string]any)["results"].([]any)
	assertTypedHTTPRow(t, results[0].(map[string]any), true)

	for _, name := range []string{"search_fulltext", "search_vector"} {
		def, ok := Ops[name]
		if !ok {
			t.Fatalf("%s op missing", name)
		}
		if _, ok := def.InputSchema["properties"].(map[string]any)["include_hidden"]; !ok {
			t.Fatalf("%s must declare include_hidden", name)
		}
	}
}

func TestQueryPaginationOverHTTP(t *testing.T) {
	srv := newTestServer(t)

	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "page",
		"table":     "items",
		"fields":    []map[string]any{{"name": "v", "type": "number"}},
	})
	if code != 200 {
		t.Fatalf("create_table failed: %d %v", code, res)
	}

	records := make([]map[string]any, 0, 5)
	for i := 0; i < 5; i++ {
		records = append(records, map[string]any{"v": i})
	}
	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace": "page",
		"table":     "items",
		"records":   records,
	})
	if code != 200 {
		t.Fatalf("insert failed: %d %v", code, res)
	}

	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "page",
		"sql":       "SELECT v FROM items ORDER BY id",
		"offset":    0,
		"limit":     2,
	})
	if code != 200 {
		t.Fatalf("query page 0 failed: %d %v", code, res)
	}
	data := res["data"].(map[string]any)
	rows := data["rows"].([]any)
	if len(rows) != 2 || data["truncated"] != true {
		t.Fatalf("page 0 should return 2 rows and truncated=true: %v", data)
	}

	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "page",
		"sql":       "SELECT v FROM items ORDER BY id",
		"offset":    4,
		"limit":     2,
	})
	if code != 200 {
		t.Fatalf("query page 2 failed: %d %v", code, res)
	}
	data = res["data"].(map[string]any)
	rows = data["rows"].([]any)
	if len(rows) != 1 || data["truncated"] != false {
		t.Fatalf("page 2 should return 1 row and truncated=false: %v", data)
	}
}
