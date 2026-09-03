package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"

	_ "modernc.org/sqlite"
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

func TestSearchVectorTextRejectedForCallerProvidedVectors(t *testing.T) {
	srv := newTestServer(t)
	code, _ := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "rv",
		"table":     "t",
		"fields": []map[string]any{
			{"name": "s", "type": "string"},
			{"name": "emb", "type": "vector", "dim": 4},
		},
	})
	if code != 200 {
		t.Fatal("create failed")
	}
	code, _ = post(t, srv.URL, "insert", map[string]any{
		"namespace": "rv",
		"table":     "t",
		"records":   []map[string]any{{"s": "hello", "emb": []float64{1, 0, 0, 0}}},
	})
	if code != 200 {
		t.Fatal("insert failed")
	}
	// Text queries must not silently compare a server embedding against
	// caller-provided vectors from an unrelated space — with or without column.
	for _, extra := range []map[string]any{
		{"text": "hello"},
		{"text": "hello", "column": "emb"},
	} {
		body := map[string]any{"namespace": "rv", "table": "t"}
		for k, v := range extra {
			body[k] = v
		}
		code, res := post(t, srv.URL, "search_vector", body)
		if code != 400 {
			t.Fatalf("text query %v against a caller-provided vector column must 400, got %d %v", extra, code, res)
		}
		if msg, _ := res["error"].(string); !strings.Contains(msg, "vectorize") {
			t.Fatalf("rejection should point at the vectorize path or a raw-vector retry, got %q", msg)
		}
	}
	// Raw vectors from the caller's own space keep working, column or not.
	for _, extra := range []map[string]any{
		{"vector": []float64{1, 0, 0, 0}},
		{"vector": []float64{1, 0, 0, 0}, "column": "emb"},
	} {
		body := map[string]any{"namespace": "rv", "table": "t"}
		for k, v := range extra {
			body[k] = v
		}
		code, res := post(t, srv.URL, "search_vector", body)
		if code != 200 {
			t.Fatalf("raw-vector query %v must stay searchable, got %d %v", extra, code, res)
		}
		results := res["data"].(map[string]any)["results"].([]any)
		if len(results) != 1 {
			t.Fatalf("expected 1 hit, got %v", results)
		}
	}
}

func TestSearchVectorReportsSkippedVectors(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, fakeEmb{}).Handler())
	t.Cleanup(srv.Close)
	code, _ := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "sk",
		"table":     "t",
		"fields": []map[string]any{
			{"name": "s", "type": "string"},
			{"name": "emb", "type": "vector", "dim": 3},
		},
	})
	if code != 200 {
		t.Fatal("create failed")
	}
	code, _ = post(t, srv.URL, "insert", map[string]any{
		"namespace": "sk",
		"table":     "t",
		"records": []map[string]any{
			{"s": "good", "emb": []float64{1, 0, 0}},
			{"s": "bad", "emb": []float64{0, 1, 0}},
		},
	})
	if code != 200 {
		t.Fatal("insert failed")
	}
	// Corrupt one row the way only an out-of-band SQLite writer could.
	raw, err := sql.Open("sqlite", filepath.Join(dir, "sk.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	if _, err := raw.Exec(`UPDATE t SET emb = X'0102' WHERE s = 'bad'`); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	code, res := post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "sk", "table": "t", "vector": []float64{1, 0, 0},
	})
	if code != 200 {
		t.Fatalf("search over partially corrupt data must succeed, got %d %v", code, res)
	}
	data := res["data"].(map[string]any)
	results := data["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["s"] != "good" {
		t.Fatalf("expected only the intact row, got %v", results)
	}
	if skipped, _ := data["skipped_vectors"].(float64); skipped != 1 {
		t.Fatalf("response must report skipped_vectors=1 so callers see results are incomplete, got %v", data["skipped_vectors"])
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

func TestUpdateAndUpsertOverHTTP(t *testing.T) {
	srv := newTestServer(t)

	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"fields": []map[string]any{
			{"name": "title", "type": "string", "fulltext": true},
			{"name": "detail", "type": "text", "vectorize": true},
			{"name": "confidence", "type": "number"},
			{"name": "done", "type": "boolean"},
		},
	})
	if code != 200 {
		t.Fatalf("create_table failed: %d %v", code, res)
	}
	code, _ = post(t, srv.URL, "insert", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"records": []map[string]any{
			{"title": "auth bug", "detail": "token expiry not checked", "confidence": 0.9},
			{"title": "slow query", "detail": "missing index", "confidence": 0.7},
			{"title": "typo", "detail": "misspelled label", "confidence": 0.5, "done": true},
		},
	})
	if code != 200 {
		t.Fatal("insert failed")
	}

	// filter + args + coercion across several matched rows
	code, res = post(t, srv.URL, "update", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"filter":    "confidence >= ?",
		"args":      []any{0.7},
		"set":       map[string]any{"done": true},
	})
	if code != 200 || res["data"].(map[string]any)["updated"].(float64) != 2 {
		t.Fatalf("update must set 2 rows: %d %v", code, res)
	}

	// fulltext index follows the new text
	code, res = post(t, srv.URL, "update", map[string]any{
		"namespace": "skills", "table": "findings",
		"filter": "title = 'slow query'",
		"set":    map[string]any{"title": "slow query fixed"},
	})
	if code != 200 || res["data"].(map[string]any)["updated"].(float64) != 1 {
		t.Fatalf("rename update failed: %d %v", code, res)
	}
	code, res = post(t, srv.URL, "search_fulltext", map[string]any{
		"namespace": "skills", "table": "findings", "query": "fixed",
	})
	if code != 200 {
		t.Fatalf("search failed: %d %v", code, res)
	}
	results := res["data"].(map[string]any)["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["title"] != "slow query fixed" {
		t.Fatalf("fts must reflect the renamed title, got %v", results)
	}

	// null clears a field
	code, _ = post(t, srv.URL, "update", map[string]any{
		"namespace": "skills", "table": "findings",
		"filter": "title = 'typo'",
		"set":    map[string]any{"confidence": nil},
	})
	if code != 200 {
		t.Fatalf("null clear must be allowed: %d", code)
	}
	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "skills", "sql": "SELECT confidence FROM findings WHERE title = 'typo'",
	})
	if code != 200 {
		t.Fatalf("query failed: %d", code)
	}
	rows := res["data"].(map[string]any)["rows"].([]any)
	if v, present := rows[0].(map[string]any)["confidence"]; present && v != nil {
		t.Fatalf("confidence must be cleared, got %v", rows[0])
	}

	// unknown field and missing set are rejected
	code, _ = post(t, srv.URL, "update", map[string]any{
		"namespace": "skills", "table": "findings",
		"filter": "1=1",
		"set":    map[string]any{"bogus": 1},
	})
	if code != 400 {
		t.Fatalf("unknown field must 400, got %d", code)
	}
	code, _ = post(t, srv.URL, "update", map[string]any{
		"namespace": "skills", "table": "findings", "set": map[string]any{"done": true},
	})
	if code != 400 {
		t.Fatalf("missing filter must 400, got %d", code)
	}

	// upsert with no match inserts (and embeds the vectorized field)
	code, res = post(t, srv.URL, "upsert", map[string]any{
		"namespace": "skills", "table": "findings",
		"filter": "title = 'ghost'",
		"set":    map[string]any{"title": "ghost finding", "detail": "haunting detail", "confidence": 0.1},
	})
	if code != 200 {
		t.Fatalf("upsert insert failed: %d %v", code, res)
	}
	data := res["data"].(map[string]any)
	if data["inserted"] != true || data["updated"].(float64) != 0 {
		t.Fatalf("expected insert result, got %v", data)
	}
	id, ok := data["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("inserted result must carry the new id, got %v", data)
	}
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "skills", "table": "findings", "text": "haunting detail",
	})
	if code != 200 {
		t.Fatalf("vector search failed: %d %v", code, res)
	}
	results = res["data"].(map[string]any)["results"].([]any)
	if len(results) != 4 || results[0].(map[string]any)["id"].(float64) != id {
		t.Fatalf("upserted row must be embedded and rank first, got %v", results)
	}

	// upsert with a match updates instead
	code, res = post(t, srv.URL, "upsert", map[string]any{
		"namespace": "skills", "table": "findings",
		"filter": "title = 'auth bug'",
		"set":    map[string]any{"confidence": 0.99},
	})
	if code != 200 {
		t.Fatalf("upsert update failed: %d %v", code, res)
	}
	data = res["data"].(map[string]any)
	if data["inserted"] != false || data["updated"].(float64) != 1 {
		t.Fatalf("expected update result, got %v", data)
	}
	if _, hasID := data["id"]; hasID {
		t.Fatalf("update result must not carry an id, got %v", data)
	}
}

func TestUpsertInsertPathRequiresRequiredFields(t *testing.T) {
	srv := newTestServer(t)
	code, _ := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "req",
		"table":     "t",
		"fields": []map[string]any{
			{"name": "name", "type": "string", "required": true},
			{"name": "note", "type": "string"},
		},
	})
	if code != 200 {
		t.Fatal("create failed")
	}
	code, _ = post(t, srv.URL, "upsert", map[string]any{
		"namespace": "req", "table": "t",
		"filter": "1=0",
		"set":    map[string]any{"note": "no name"},
	})
	if code != 400 {
		t.Fatalf("upsert insert path must enforce required fields, got %d", code)
	}
	code, res := post(t, srv.URL, "query", map[string]any{
		"namespace": "req", "sql": "SELECT count(*) AS n FROM t",
	})
	if code != 200 {
		t.Fatalf("query failed: %d", code)
	}
	if res["data"].(map[string]any)["rows"].([]any)[0].(map[string]any)["n"].(float64) != 0 {
		t.Fatal("failed upsert must not leave a row behind")
	}
}

func TestUpdateUpsertSchemaParity(t *testing.T) {
	for _, name := range []string{"update", "upsert"} {
		def, ok := Ops[name]
		if !ok {
			t.Fatalf("%s op missing", name)
		}
		props := def.InputSchema["properties"].(map[string]any)
		required := def.InputSchema["required"].([]string)
		if len(required) != 4 || required[3] != "set" {
			t.Fatalf("%s must require namespace, table, filter, set, got %v", name, required)
		}
		filter := props["filter"].(map[string]any)
		if filter["pattern"] != `\S` {
			t.Fatalf("%s filter must require a non-whitespace character, got %v", name, filter)
		}
		if _, ok := filter["not"].(map[string]any)["pattern"]; !ok {
			t.Fatalf("%s filter must exclude all semicolons, got %v", name, filter)
		}
		set := props["set"].(map[string]any)
		if set["type"] != "object" || set["minProperties"] != 1 {
			t.Fatalf("%s set must be a non-empty object, got %v", name, set)
		}
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
	if sqlP["pattern"] != `^\s*([sS][eE][lL][eE][cC][tT]|[wW][iI][tT][hH])\b[\s\S]*$` {
		t.Fatalf("sql must anchor to a SELECT/WITH prefix without banning semicolons (store guard is authoritative), got %v", sqlP["pattern"])
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
	if _, ok := filter["not"]; ok {
		t.Fatalf("filter must not ban semicolons outright (store guard is authoritative), got %v", filter)
	}
	sv, ok := Ops["search_vector"]
	if !ok {
		t.Fatal("search_vector op missing")
	}
	vecItems := sv.InputSchema["properties"].(map[string]any)["vector"].(map[string]any)["items"].(map[string]any)
	if vecItems["maximum"].(float64) != 3.4028234663852886e+38 || vecItems["minimum"].(float64) != -3.4028234663852886e+38 {
		t.Fatalf("vector items must declare the float32 range, got %v", vecItems)
	}
	svFilter := sv.InputSchema["properties"].(map[string]any)["filter"].(map[string]any)
	if svFilter["pattern"] != `\S` {
		t.Fatalf("search_vector filter must require a non-whitespace character, got %v", svFilter)
	}
	if _, ok := svFilter["not"].(map[string]any)["pattern"]; !ok {
		t.Fatalf("search_vector filter must exclude all semicolons, got %v", svFilter)
	}
	svArgs := sv.InputSchema["properties"].(map[string]any)["args"].(map[string]any)
	if svArgs["maxItems"] != 100 {
		t.Fatalf("search_vector args must declare maxItems 100, got %v", svArgs)
	}
	svMinScore := sv.InputSchema["properties"].(map[string]any)["min_score"].(map[string]any)
	if svMinScore["type"] != "number" {
		t.Fatalf("search_vector min_score must be a number, got %v", svMinScore)
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

func TestSemicolonInsideQuotesAllowedAtAPI(t *testing.T) {
	srv := newTestServer(t)

	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "ns",
		"table":     "findings",
		"fields":    []map[string]any{{"name": "title", "type": "string"}},
	})
	if code != 200 {
		t.Fatalf("create_table failed: %d %v", code, res)
	}
	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace": "ns",
		"table":     "findings",
		"records": []map[string]any{
			{"title": "a;b"},
			{"title": "plain"},
		},
	})
	if code != 200 {
		t.Fatalf("insert failed: %d %v", code, res)
	}

	// A semicolon inside a quoted literal reaches the store and matches.
	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "ns", "sql": "SELECT title FROM findings WHERE title = 'a;b'",
	})
	if code != 200 {
		t.Fatalf("query with semicolon literal failed: %d %v", code, res)
	}
	rows := res["data"].(map[string]any)["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["title"] != "a;b" {
		t.Fatalf("unexpected query rows: %v", rows)
	}

	// A genuine multi-statement query is still rejected by the store.
	code, _ = post(t, srv.URL, "query", map[string]any{
		"namespace": "ns", "sql": "SELECT 1; SELECT 2",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for multi-statement query, got %d", code)
	}

	// Same for the delete filter.
	code, res = post(t, srv.URL, "delete", map[string]any{
		"namespace": "ns", "table": "findings", "filter": "title = 'a;b'",
	})
	if code != 200 || res["data"].(map[string]any)["deleted"].(float64) != 1 {
		t.Fatalf("delete with semicolon filter failed: %d %v", code, res)
	}
	code, _ = post(t, srv.URL, "delete", map[string]any{
		"namespace": "ns", "table": "findings", "filter": "title = 'a;b'; DROP TABLE findings",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for multi-statement filter, got %d", code)
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
	if skipped, _ := res["data"].(map[string]any)["skipped_vectors"].(float64); skipped != 0 {
		t.Fatalf("healthy table must report skipped_vectors=0, got %v", res["data"].(map[string]any)["skipped_vectors"])
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

func TestSearchVectorFilterAndMinScoreOverHTTP(t *testing.T) {
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

	// filter with bound arg before scoring
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"text":      "token expiry not checked in middleware",
		"filter":    "confidence >= ?",
		"args":      []any{0.8},
	})
	if code != 200 {
		t.Fatalf("filtered search_vector failed: %d %v", code, res)
	}
	results := res["data"].(map[string]any)["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["title"] != "auth bug" {
		t.Fatalf("expected auth bug only, got %v", results)
	}

	// min_score threshold before ranking/limit
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"text":      "token expiry not checked in middleware",
		"min_score": 0.99,
	})
	if code != 200 {
		t.Fatalf("min_score search_vector failed: %d %v", code, res)
	}
	results = res["data"].(map[string]any)["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["title"] != "auth bug" {
		t.Fatalf("expected one high-confidence hit, got %v", results)
	}

	// filter + min_score together
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"text":      "token expiry not checked in middleware",
		"filter":    "confidence >= ?",
		"args":      []any{0.85},
		"min_score": 0.99,
	})
	if code != 200 {
		t.Fatalf("filter+min_score search_vector failed: %d %v", code, res)
	}
	results = res["data"].(map[string]any)["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["title"] != "auth bug" {
		t.Fatalf("expected auth bug with combined constraints, got %v", results)
	}

	// null bind arguments are allowed in filter args
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"text":      "token expiry not checked in middleware",
		"filter":    "1=1",
		"args":      []any{nil},
	})
	if code != 200 {
		t.Fatalf("null in filter args must be accepted, got %d %v", code, res)
	}

	// null vector entries are still rejected
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"vector":    []any{1, nil, 0, 0},
	})
	if code != 400 {
		t.Fatalf("null vector entries must 400, got %d %v", code, res)
	}

	// invalid filter is rejected
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"text":      "anything",
		"filter":    "1=1; DROP TABLE findings",
	})
	if code != 400 {
		t.Fatalf("semicolon in filter must 400, got %d %v", code, res)
	}
}
