package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/lsm/dolmen"
)

func canonical(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		return t
	case int64:
		return t
	case int:
		return int64(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = canonical(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = canonical(x)
		}
		return out
	default:
		return v
	}
}

func numberKey(v any) (string, bool) {
	switch t := v.(type) {
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return strconv.FormatInt(i, 10), true
		}
		if f, err := t.Float64(); err == nil {
			return strconv.FormatFloat(f, 'g', -1, 64), true
		}
		return t.String(), true
	}
	return "", false
}

func closeNumbers(a, b any) bool {
	ka, aok := numberKey(a)
	kb, bok := numberKey(b)
	if aok && bok {
		return ka == kb
	}
	return reflect.DeepEqual(a, b)
}

func rowsEqual(t *testing.T, label string, httpRows, embeddedRows []map[string]any) {
	t.Helper()
	if len(httpRows) != len(embeddedRows) {
		t.Fatalf("%s: row count differs: http %d vs embedded %d", label, len(httpRows), len(embeddedRows))
	}
	for i := range httpRows {
		h := canonical(httpRows[i]).(map[string]any)
		e := canonical(embeddedRows[i]).(map[string]any)
		delete(h, "created_at")
		delete(e, "created_at")
		if len(h) != len(e) {
			t.Fatalf("%s row %d: field sets differ: %v vs %v", label, i, h, e)
		}
		for k, hv := range h {
			ev, ok := e[k]
			if !ok {
				t.Fatalf("%s row %d: field %q missing embedded", label, i, k)
			}
			if !closeNumbers(hv, ev) {
				t.Fatalf("%s row %d: field %q differs: %v (%T) vs %v (%T)", label, i, k, hv, hv, ev, ev)
			}
		}
	}
}

func openEmbedded(t *testing.T, dir string, opts ...dolmen.Option) *dolmen.Store {
	t.Helper()
	st, err := dolmen.Open(dir, append([]dolmen.Option{dolmen.WithEngine(testEngine(t))}, opts...)...)
	if err != nil {
		t.Fatalf("embedded open: %v", err)
	}
	return st
}

func embeddedStore(t *testing.T) (*dolmen.Store, dolmen.EmbeddingProvider) {
	t.Helper()
	emb := &parityProvider{}
	st := openEmbedded(t, t.TempDir(), dolmen.WithEmbedding(emb))
	t.Cleanup(func() { st.Close() })
	return st, emb
}

type parityProvider struct {
	fail error
}

func (parityProvider) Identity() string { return "conformance|fake|v1" }

func (parityProvider) Name() string      { return "conformance" }
func (parityProvider) ModelName() string { return "fake-model" }

func (p parityProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	out := make([][]float32, len(texts))
	for i := range out {
		v := make([]float32, 8)
		for j, r := range []byte(texts[i]) {
			v[r%8] += float32(j + 1)
		}
		if texts[i] == "" {
			v[0] = 1
		}
		out[i] = v
	}
	return out, nil
}

func (p parityProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	vecs, err := p.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

func seedParityTableHTTP(t *testing.T, h *harness) {
	t.Helper()
	h.seedTable("par", "notes", []map[string]any{
		{"name": "body", "type": "text", "fulltext": true},
		{"name": "score", "type": "number"},
		{"name": "done", "type": "boolean"},
		{"name": "tags", "type": "json"},
		{"name": "rank", "type": "number", "default": 7},
	})
}

func seedParityTableEmbedded(t *testing.T, st *dolmen.Store) {
	t.Helper()
	_, err := st.CreateTable(context.Background(), "par", "notes", []dolmen.Field{
		{Name: "body", Type: dolmen.Text, Fulltext: true},
		{Name: "score", Type: dolmen.Number},
		{Name: "done", Type: dolmen.Boolean},
		{Name: "tags", Type: dolmen.JSON},
		{Name: "rank", Type: dolmen.Number, Default: 7},
	})
	if err != nil {
		t.Fatalf("embedded create table: %v", err)
	}
}

var parityRecords = []map[string]any{
	{"body": "alpha note", "score": 1, "done": true, "tags": []any{"x", 1}},
	{"body": "beta note", "score": 2.5, "done": false, "tags": nil},
	{"body": "gamma note", "score": 3, "done": true},
}

func TestEmbeddedParityValuesCoercionAndDefaults(t *testing.T) {
	h := newHarness(t)
	seedParityTableHTTP(t, h)
	httpRes := h.mustHTTPNumbered("insert", map[string]any{
		"namespace": "par", "table": "notes", "records": parityRecords,
	})

	st, _ := embeddedStore(t)
	seedParityTableEmbedded(t, st)
	embRes, err := st.Insert(context.Background(), "par", "notes", parityRecords, dolmen.InsertOptions{})
	if err != nil {
		t.Fatalf("embedded insert: %v", err)
	}

	httpIDs, _ := httpRes["ids"].([]any)
	if len(httpIDs) != len(embRes.Ids) {
		t.Fatalf("id counts differ: %v vs %v", httpIDs, embRes.Ids)
	}

	httpRows := h.mustHTTPNumbered("read_rows", map[string]any{
		"namespace": "par", "table": "notes", "ids": httpIDs,
	})
	hRows, _ := httpRows["rows"].([]any)
	var httpTyped []map[string]any
	for _, r := range hRows {
		httpTyped = append(httpTyped, r.(map[string]any))
	}
	embRows, err := st.GetRows(context.Background(), "par", "notes", embRes.Ids)
	if err != nil {
		t.Fatalf("embedded read: %v", err)
	}
	rowsEqual(t, "coercion", httpTyped, embRows.Rows)
}

func TestEmbeddedParityPaginationAndFilters(t *testing.T) {
	h := newHarness(t)
	seedParityTableHTTP(t, h)
	h.mustHTTPNumbered("insert", map[string]any{"namespace": "par", "table": "notes", "records": parityRecords})

	st, _ := embeddedStore(t)
	seedParityTableEmbedded(t, st)
	if _, err := st.Insert(context.Background(), "par", "notes", parityRecords, dolmen.InsertOptions{}); err != nil {
		t.Fatalf("embedded insert: %v", err)
	}

	sql := "SELECT id, body, score FROM notes WHERE score >= ? ORDER BY score"
	httpPage := h.mustHTTPNumbered("query", map[string]any{
		"namespace": "par", "sql": sql, "args": []any{2}, "limit": 1,
	})
	embPage, err := st.Query(context.Background(), "par", sql, dolmen.QueryOptions{Args: []any{2}, Limit: 1})
	if err != nil {
		t.Fatalf("embedded query: %v", err)
	}
	if httpPage["truncated"] != true || embPage.Truncated != true {
		t.Fatalf("a limit-1 page over two matches must report truncated=true on both surfaces: http %v vs embedded %v", httpPage["truncated"], embPage.Truncated)
	}
	hRows, _ := httpPage["rows"].([]any)
	var httpTyped []map[string]any
	for _, r := range hRows {
		httpTyped = append(httpTyped, r.(map[string]any))
	}
	rowsEqual(t, "pagination", httpTyped, embPage.Rows)

	httpSecond := h.mustHTTPNumbered("query", map[string]any{
		"namespace": "par", "sql": sql, "args": []any{2}, "limit": 1, "offset": 1,
	})
	embSecond, err := st.Query(context.Background(), "par", sql, dolmen.QueryOptions{Args: []any{2}, Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("embedded query page two: %v", err)
	}
	hRows2, _ := httpSecond["rows"].([]any)
	var httpTyped2 []map[string]any
	for _, r := range hRows2 {
		httpTyped2 = append(httpTyped2, r.(map[string]any))
	}
	rowsEqual(t, "pagination page two", httpTyped2, embSecond.Rows)
}

func TestEmbeddedParityIdempotencyAndConflicts(t *testing.T) {
	h := newHarness(t)
	seedParityTableHTTP(t, h)
	h.mustHTTPNumbered("insert", map[string]any{
		"namespace": "par", "table": "notes",
		"records":         []map[string]any{parityRecords[0]},
		"idempotency_key": "retry-1",
	})
	replay := h.mustHTTPNumbered("insert", map[string]any{
		"namespace": "par", "table": "notes",
		"records":         []map[string]any{parityRecords[0]},
		"idempotency_key": "retry-1",
	})

	st, _ := embeddedStore(t)
	seedParityTableEmbedded(t, st)
	ctx := context.Background()
	if _, err := st.Insert(ctx, "par", "notes", parityRecords[:1], dolmen.InsertOptions{IdempotencyKey: "retry-1"}); err != nil {
		t.Fatalf("embedded insert: %v", err)
	}
	embReplay, err := st.Insert(ctx, "par", "notes", parityRecords[:1], dolmen.InsertOptions{IdempotencyKey: "retry-1"})
	if err != nil {
		t.Fatalf("embedded replay: %v", err)
	}
	if replay["replayed"] != embReplay.Replayed || replay["replayed"] != true {
		t.Fatalf("replay flags differ: %v vs %v", replay["replayed"], embReplay.Replayed)
	}

	status, diverged := h.httpCallNumbered("insert", map[string]any{
		"namespace": "par", "table": "notes",
		"records":         []map[string]any{parityRecords[1]},
		"idempotency_key": "retry-1",
	})
	errObj, _ := diverged["error"].(map[string]any)
	if status != 400 || errObj["code"] != "conflict" {
		t.Fatalf("http divergence: %d %v", status, diverged)
	}
	_, embErr := st.Insert(ctx, "par", "notes", parityRecords[1:2], dolmen.InsertOptions{IdempotencyKey: "retry-1"})
	if !errors.Is(embErr, dolmen.ErrConflict) {
		t.Fatalf("embedded divergence must be a typed conflict, got %v", embErr)
	}
}

func TestEmbeddedParityErrorTaxonomy(t *testing.T) {
	h := newHarness(t)
	seedParityTableHTTP(t, h)

	st, _ := embeddedStore(t)
	seedParityTableEmbedded(t, st)
	ctx := context.Background()

	status, body := h.httpCallNumbered("describe_table", map[string]any{"namespace": "par", "table": "missing"})
	errObj, _ := body["error"].(map[string]any)
	if status != 404 || errObj["code"] != "not_found" {
		t.Fatalf("http missing table: %d %v", status, body)
	}
	if _, _, err := st.DescribeTable(ctx, "par", "missing"); !errors.Is(err, dolmen.ErrNotFound) {
		t.Fatalf("embedded missing table must be not_found, got %v", err)
	}

	status, body = h.httpCallNumbered("query", map[string]any{"namespace": "par", "sql": "SELECT missing_col FROM notes"})
	errObj, _ = body["error"].(map[string]any)
	if status != 400 || errObj["code"] != "query_error" {
		t.Fatalf("http bad column: %d %v", status, body)
	}
	if _, err := st.Query(ctx, "par", "SELECT missing_col FROM notes", dolmen.QueryOptions{}); !errors.Is(err, dolmen.ErrQuery) {
		t.Fatalf("embedded bad column must be query_error, got %v", err)
	}

	status, body = h.httpCallNumbered("insert", map[string]any{
		"namespace": "par", "table": "notes",
		"records": []map[string]any{{"body": "x", "unknown_field": 1}},
	})
	errObj, _ = body["error"].(map[string]any)
	if status != 400 || errObj["code"] != "invalid_request" {
		t.Fatalf("http unknown field: %d %v", status, body)
	}
	if _, err := st.Insert(ctx, "par", "notes", []map[string]any{{"unknown_field": 1}}, dolmen.InsertOptions{}); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("embedded unknown field must be invalid_request, got %v", err)
	}

	status, body = h.httpCallNumbered("describe_table", map[string]any{"namespace": "par", "table": "sqlite_notes"})
	errObj, _ = body["error"].(map[string]any)
	if status != 404 || errObj["code"] != "not_found" {
		t.Fatalf("http malformed table name (the /v1 surface does not enforce the MCP grammar): %d %v", status, body)
	}
	if _, _, err := st.DescribeTable(ctx, "par", "sqlite_notes"); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("embedded malformed table name is rejected invalid_request by the curated façade (by-design stricter than /v1, which reports not_found), got %v", err)
	}
	if err := st.DropTable(ctx, "par", "sqlite_notes"); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("embedded malformed table name on drop is rejected invalid_request by the curated façade, got %v", err)
	}
}

func TestEmbeddedParityEmbeddingIdentityAndSearch(t *testing.T) {
	h := newHarness(t)
	h.seedTable("par", "docs", []map[string]any{{"name": "body", "type": "text", "fulltext": true, "vectorize": true}})
	h.mustHTTPNumbered("insert", map[string]any{
		"namespace": "par", "table": "docs",
		"records": []map[string]any{
			{"body": "refund processed"},
			{"body": "payment pending"},
		},
	})

	st, emb := embeddedStore(t)
	_ = emb
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "par", "docs", []dolmen.Field{{Name: "body", Type: dolmen.Text, Fulltext: true, Vectorize: true}}); err != nil {
		t.Fatalf("embedded create: %v", err)
	}
	if _, err := st.Insert(ctx, "par", "docs", []map[string]any{
		{"body": "refund processed"},
		{"body": "payment pending"},
	}, dolmen.InsertOptions{}); err != nil {
		t.Fatalf("embedded insert: %v", err)
	}

	described, _, err := st.DescribeTable(ctx, "par", "docs")
	if err != nil {
		t.Fatalf("embedded describe: %v", err)
	}
	if described.EmbedSpace != "conformance|fake|v1" {
		t.Fatalf("embedded table must pin the provider identity, got %q", described.EmbedSpace)
	}
	httpDesc := h.mustHTTPNumbered("describe_table", map[string]any{"namespace": "par", "table": "docs"})
	httpTable, _ := httpDesc["table"].(map[string]any)
	if httpTable["embed_space"] != "conformance|fake|v1" {
		t.Fatalf("the wire must pin the provider identity in table.embed_space, got %v", httpTable["embed_space"])
	}

	httpSearch := h.mustHTTPNumbered("search_vector", map[string]any{
		"namespace": "par", "table": "docs", "text": "refund",
	})
	hResults, _ := httpSearch["results"].([]any)
	embSearch, err := st.SearchVector(ctx, "par", "docs", dolmen.VectorQuery{Text: "refund"}, dolmen.SearchOptions{})
	if err != nil {
		t.Fatalf("embedded vector search: %v", err)
	}
	if len(hResults) != len(embSearch.Rows) {
		t.Fatalf("vector result counts differ: %d vs %d", len(hResults), len(embSearch.Rows))
	}
	var httpTyped []map[string]any
	for _, r := range hResults {
		httpTyped = append(httpTyped, r.(map[string]any))
	}
	rowsEqual(t, "vector search", httpTyped, embSearch.Rows)

	httpFTS := h.mustHTTPNumbered("search_fulltext", map[string]any{
		"namespace": "par", "table": "docs", "query": "refunds",
	})
	hFTS, _ := httpFTS["results"].([]any)
	embFTS, err := st.SearchFulltext(ctx, "par", "docs", "refunds", dolmen.SearchOptions{})
	if err != nil {
		t.Fatalf("embedded fulltext search: %v", err)
	}
	if len(hFTS) != len(embFTS.Rows) {
		t.Fatalf("fulltext result counts differ: %d vs %d", len(hFTS), len(embFTS.Rows))
	}
	var httpTypedFTS []map[string]any
	for _, r := range hFTS {
		httpTypedFTS = append(httpTypedFTS, r.(map[string]any))
	}
	rowsEqual(t, "fulltext search", httpTypedFTS, embFTS.Rows)

	httpTrunc := h.mustHTTPNumbered("search_vector", map[string]any{
		"namespace": "par", "table": "docs", "text": "refund", "limit": 1,
	})
	embTrunc, err := st.SearchVector(ctx, "par", "docs", dolmen.VectorQuery{Text: "refund"}, dolmen.SearchOptions{Limit: 1})
	if err != nil {
		t.Fatalf("embedded vector truncation probe: %v", err)
	}
	if httpTrunc["truncated"] != true || embTrunc.Truncated != true {
		t.Fatalf("vector search with two matches and limit 1 must report truncated=true on both surfaces: http %v vs embedded %v", httpTrunc["truncated"], embTrunc.Truncated)
	}

	httpFTSTrunc := h.mustHTTPNumbered("search_fulltext", map[string]any{
		"namespace": "par", "table": "docs", "query": "refund OR payment", "limit": 1,
	})
	embFTSTrunc, err := st.SearchFulltext(ctx, "par", "docs", "refund OR payment", dolmen.SearchOptions{Limit: 1})
	if err != nil {
		t.Fatalf("embedded fulltext truncation probe: %v", err)
	}
	if httpFTSTrunc["truncated"] != true || embFTSTrunc.Truncated != true {
		t.Fatalf("fulltext search with two matches and limit 1 must report truncated=true on both surfaces: http %v vs embedded %v", httpFTSTrunc["truncated"], embFTSTrunc.Truncated)
	}
}

func TestEmbeddedParityEmbeddingFailure(t *testing.T) {
	h := newHarness(t)
	h.seedTable("par", "docs", []map[string]any{{"name": "body", "type": "text", "vectorize": true}})
	h.mustHTTPNumbered("insert", map[string]any{
		"namespace": "par", "table": "docs", "records": []map[string]any{{"body": "seed"}},
	})
	h.emb.mu.Lock()
	h.emb.fail = errors.New("provider exploded")
	h.emb.mu.Unlock()
	wStatus, wBody := h.httpCallNumbered("insert", map[string]any{
		"namespace": "par", "table": "docs", "records": []map[string]any{{"body": "never embedded"}},
	})
	wErrObj, _ := wBody["error"].(map[string]any)
	if wStatus != 500 || wErrObj["code"] != "internal_error" {
		t.Fatalf("http insert through a failing provider must keep the pinned 500 internal_error shape: %d %v", wStatus, wBody)
	}
	status, body := h.httpCallNumbered("search_vector", map[string]any{
		"namespace": "par", "table": "docs", "text": "query",
	})
	errObj, _ := body["error"].(map[string]any)
	if status != 500 || errObj["code"] != "internal_error" {
		t.Fatalf("http custom-provider failure keeps its pinned wire shape: %d %v", status, body)
	}

	boom := errors.New("provider exploded")
	st := openEmbedded(t, t.TempDir(), dolmen.WithEmbedding(&parityProvider{fail: boom}))
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "par", "docs", []dolmen.Field{{Name: "body", Type: dolmen.Text, Vectorize: true}}); err != nil {
		t.Fatalf("embedded create: %v", err)
	}
	embErr := func() error {
		_, err := st.Insert(ctx, "par", "docs", []map[string]any{{"body": "seed"}}, dolmen.InsertOptions{})
		return err
	}()
	if embErr == nil {
		t.Fatal("embedded write through a failing provider must fail")
	}
	if !errors.Is(embErr, boom) {
		t.Fatalf("embedded failure must keep its cause, got %v", embErr)
	}
	if !errors.Is(embErr, dolmen.ErrEmbedderUnavailable) {
		t.Fatalf("embedded failure must classify embedder_unavailable, got %v", embErr)
	}
	_, searchErr := st.SearchVector(ctx, "par", "docs", dolmen.VectorQuery{Text: "query"}, dolmen.SearchOptions{})
	if !errors.Is(searchErr, dolmen.ErrEmbedderUnavailable) || !errors.Is(searchErr, boom) {
		t.Fatalf("embedded search failure must classify the same way, got %v", searchErr)
	}
}

func TestEmbeddedParityEmbedQueryFailureThroughSearch(t *testing.T) {
	h := newHarness(t)
	h.seedTable("par", "docs", []map[string]any{{"name": "body", "type": "text", "vectorize": true}})
	h.mustHTTPNumbered("insert", map[string]any{
		"namespace": "par", "table": "docs", "records": []map[string]any{{"body": "seed"}},
	})
	h.emb.mu.Lock()
	h.emb.fail = errors.New("query embedder down")
	h.emb.mu.Unlock()
	status, body := h.httpCallNumbered("search_vector", map[string]any{
		"namespace": "par", "table": "docs", "text": "probe",
	})
	errObj, _ := body["error"].(map[string]any)
	if status != 500 || errObj["code"] != "internal_error" {
		t.Fatalf("http text-query provider failure keeps its pinned wire shape: %d %v", status, body)
	}

	boom := errors.New("query embedder down")
	emb := &parityProvider{}
	st := openEmbedded(t, t.TempDir(), dolmen.WithEmbedding(emb))
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "par", "docs", []dolmen.Field{{Name: "body", Type: dolmen.Text, Vectorize: true}}); err != nil {
		t.Fatalf("embedded create: %v", err)
	}
	if _, err := st.Insert(ctx, "par", "docs", []map[string]any{{"body": "seed"}}, dolmen.InsertOptions{}); err != nil {
		t.Fatalf("embedded seed insert must succeed before the provider is failed: %v", err)
	}
	emb.fail = boom
	_, err := st.SearchVector(ctx, "par", "docs", dolmen.VectorQuery{Text: "probe"}, dolmen.SearchOptions{})
	if err == nil {
		t.Fatal("a text query through a failing provider must fail")
	}
	if !errors.Is(err, dolmen.ErrEmbedderUnavailable) || !errors.Is(err, boom) {
		t.Fatalf("the EmbedQuery path must classify embedder_unavailable with its cause preserved, got %v", err)
	}
}

func (h *harness) httpCallNumbered(op string, body any) (int, map[string]any) {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("marshal %s body: %v", op, err)
	}
	res := h.postWithHeaders(h.httpURL+"/"+op, raw, nil)
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		h.t.Fatalf("decode /v1/%s response: %v", op, err)
	}
	return res.StatusCode, out
}

func (h *harness) mustHTTPNumbered(op string, body any) map[string]any {
	h.t.Helper()
	status, out := h.httpCallNumbered(op, body)
	if status != http.StatusOK || out["ok"] != true {
		h.t.Fatalf("/v1/%s failed: status %d %v", op, status, out)
	}
	data, ok := out["data"].(map[string]any)
	if !ok {
		h.t.Fatalf("/v1/%s returned no data object: %v", op, out)
	}
	return data
}

func TestEmbeddedParityExactIntegerPrecision(t *testing.T) {
	h := newHarness(t)
	h.seedTable("par", "nums", []map[string]any{{"name": "big", "type": "number"}})
	httpRes := h.mustHTTPNumbered("insert", map[string]any{
		"namespace": "par", "table": "nums",
		"records": []map[string]any{{"big": json.Number("9007199254740993")}},
	})
	httpIDs, _ := httpRes["ids"].([]any)
	httpRows := h.mustHTTPNumbered("read_rows", map[string]any{
		"namespace": "par", "table": "nums", "ids": httpIDs,
	})
	hRow := httpRows["rows"].([]any)[0].(map[string]any)
	hv := canonical(hRow["big"])
	hInt, ok := hv.(int64)
	if !ok || hInt != 9007199254740993 {
		t.Fatalf("the wire must return 2^53+1 exactly, got %v (%T)", hv, hv)
	}

	st := openEmbedded(t, t.TempDir())
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "par", "nums", []dolmen.Field{{Name: "big", Type: dolmen.Number}}); err != nil {
		t.Fatalf("embedded create: %v", err)
	}
	ins, err := st.Insert(ctx, "par", "nums", []map[string]any{{"big": int64(9007199254740993)}}, dolmen.InsertOptions{})
	if err != nil {
		t.Fatalf("embedded insert: %v", err)
	}
	embRows, err := st.GetRows(ctx, "par", "nums", ins.Ids)
	if err != nil {
		t.Fatalf("embedded read: %v", err)
	}
	ev := canonical(embRows.Rows[0]["big"])
	eInt, ok := ev.(int64)
	if !ok || eInt != 9007199254740993 {
		t.Fatalf("the embedded read must return 2^53+1 exactly, got %v (%T)", ev, ev)
	}
}

func TestNumericFidelityMatrix(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fid", "nums", []map[string]any{
		{"name": "scalar", "type": "number"},
		{"name": "blob", "type": "json"},
	})
	high := "0.1234567890123456789012345"
	adjacent := "0.1234567890123456789012346"
	httpRes := h.mustHTTPNumbered("insert", map[string]any{
		"namespace": "fid", "table": "nums",
		"records": []map[string]any{
			{"scalar": json.Number(high), "blob": map[string]any{"pi": json.Number("3.141592653589793238462643383279"), "e": json.Number("1e-400")}},
			{"scalar": json.Number(adjacent)},
			{"scalar": json.Number("-0")},
			{"scalar": json.Number("9223372036854775807")},
			{"scalar": json.Number("-9223372036854775808")},
			{"scalar": json.Number("0.00001")},
			{"scalar": json.Number("1e-7")},
		},
	})
	httpIDs, _ := httpRes["ids"].([]any)
	httpRows := h.mustHTTPNumbered("read_rows", map[string]any{
		"namespace": "fid", "table": "nums", "ids": httpIDs,
	})["rows"].([]any)

	var highF float64
	if err := json.Unmarshal([]byte(high), &highF); err != nil {
		t.Fatalf("unmarshal high: %v", err)
	}
	var adjF float64
	if err := json.Unmarshal([]byte(adjacent), &adjF); err != nil {
		t.Fatalf("unmarshal adjacent: %v", err)
	}
	st := openEmbedded(t, t.TempDir())
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "fid", "nums", []dolmen.Field{
		{Name: "scalar", Type: dolmen.Number},
		{Name: "blob", Type: dolmen.JSON},
	}); err != nil {
		t.Fatalf("embedded create: %v", err)
	}
	ins, err := st.Insert(ctx, "fid", "nums", []map[string]any{
		{"scalar": highF, "blob": map[string]any{"pi": json.Number("3.141592653589793238462643383279"), "e": json.Number("1e-400")}},
		{"scalar": adjF},
		{"scalar": math.Copysign(0, -1)},
		{"scalar": int64(9223372036854775807)},
		{"scalar": int64(-9223372036854775808)},
		{"scalar": 0.00001},
		{"scalar": 1e-7},
	}, dolmen.InsertOptions{})
	if err != nil {
		t.Fatalf("embedded insert: %v", err)
	}
	embRows, err := st.GetRows(ctx, "fid", "nums", ins.Ids)
	if err != nil {
		t.Fatalf("embedded read: %v", err)
	}

	rowAt := func(rows []map[string]any, i int) map[string]any { return rows[i] }
	httpTyped := make([]map[string]any, len(httpRows))
	for i, r := range httpRows {
		httpTyped[i] = r.(map[string]any)
	}
	rowsEqual(t, "fidelity matrix", httpTyped, embRows.Rows)

	r0 := rowAt(embRows.Rows, 0)
	pi, ok := blobNumber(t, r0["blob"], "pi")
	if !ok || pi != "3.141592653589793238462643383279" {
		t.Fatalf("class (c) verbatim blob: pi digits must survive exactly, got %q", pi)
	}
	e, ok := blobNumber(t, r0["blob"], "e")
	if !ok || e != "1e-400" {
		t.Fatalf("class (c) verbatim blob: below-range exponent must survive verbatim, got %q", e)
	}
	s0 := numberKeyOf(t, httpTyped[0]["scalar"])
	if s0 != "0.12345678901234568" {
		t.Fatalf("class (b) shortest-round-trip: expected the 17-digit shortest form, got %q", s0)
	}
	k0 := numberKeyOf(t, httpTyped[0]["scalar"])
	k1 := numberKeyOf(t, httpTyped[1]["scalar"])
	if k0 != k1 {
		t.Fatalf("class (b) documented conflation: adjacent decimals beyond float64 precision must store to the same double on both surfaces, got %q vs %q", k0, k1)
	}
	if got := numberKeyOf(t, httpTyped[2]["scalar"]); got != "0" {
		t.Fatalf("negative zero must normalize to zero, got %q", got)
	}
	if got := numberKeyOf(t, httpTyped[3]["scalar"]); got != "9223372036854775807" {
		t.Fatalf("class (a) int64 max must round-trip exactly, got %q", got)
	}
	if got := numberKeyOf(t, httpTyped[4]["scalar"]); got != "-9223372036854775808" {
		t.Fatalf("class (a) int64 min must round-trip exactly, got %q", got)
	}
	if got := numberKeyOf(t, embRows.Rows[0]["scalar"]); got != "0.12345678901234568" {
		t.Fatalf("class (b) shortest-round-trip must hold on the embedded surface too, got %q", got)
	}
	if got := numberKeyOf(t, embRows.Rows[2]["scalar"]); got != "0" {
		t.Fatalf("negative zero must normalize to zero on the embedded surface too, got %q", got)
	}
	if _, ok := embRows.Rows[2]["scalar"].(int64); !ok {
		t.Fatalf("negative zero must read back as the int64 storage class (NUMERIC affinity rewrites fractionless REALs to INTEGER at storage), got %T", embRows.Rows[2]["scalar"])
	}
	if got := numberKeyOf(t, embRows.Rows[3]["scalar"]); got != "9223372036854775807" {
		t.Fatalf("class (a) int64 max must round-trip exactly on the embedded surface too, got %q", got)
	}
	if got := numberKeyOf(t, embRows.Rows[4]["scalar"]); got != "-9223372036854775808" {
		t.Fatalf("class (a) int64 min must round-trip exactly on the embedded surface too, got %q", got)
	}
	h0 := rowAt(httpTyped, 0)
	hPi, ok := blobNumber(t, h0["blob"], "pi")
	if !ok || hPi != "3.141592653589793238462643383279" {
		t.Fatalf("class (c) verbatim blob digits must survive exactly on the wire too, got %q", hPi)
	}
	hE, ok := blobNumber(t, h0["blob"], "e")
	if !ok || hE != "1e-400" {
		t.Fatalf("class (c) below-range exponent must survive verbatim on the wire too, got %q", hE)
	}
	if got := numberKeyOf(t, httpTyped[5]["scalar"]); got != "1e-05" {
		t.Fatalf("the [1e-6,1e-4) band (wire json decimal form) must reconcile to the 'g' canonical, got %q", got)
	}
	if got := numberKeyOf(t, embRows.Rows[5]["scalar"]); got != "1e-05" {
		t.Fatalf("the [1e-6,1e-4) band (embedded float64) must reconcile to the 'g' canonical, got %q", got)
	}
	if got := numberKeyOf(t, httpTyped[6]["scalar"]); got != "1e-07" {
		t.Fatalf("the below-1e-6 band (wire exponent-stripped json form) must reconcile to the 'g' canonical, got %q", got)
	}
	if got := numberKeyOf(t, embRows.Rows[6]["scalar"]); got != "1e-07" {
		t.Fatalf("the below-1e-6 band (embedded float64) must reconcile to the 'g' canonical, got %q", got)
	}

	acc, err := st.Insert(ctx, "fid", "nums", []map[string]any{{"scalar": json.Number("2.5")}}, dolmen.InsertOptions{})
	if err != nil {
		t.Fatalf("number fields ACCEPT json.Number (parsed to its numeric value): %v", err)
	}
	accRows, err := st.GetRows(ctx, "fid", "nums", acc.Ids)
	if err != nil {
		t.Fatalf("read json.Number insert: %v", err)
	}
	if k := numberKeyOf(t, accRows.Rows[0]["scalar"]); k != "2.5" {
		t.Fatalf("json.Number scalar must coerce to its numeric value, got %q", k)
	}
	wStatus, wBody := h.httpCallNumbered("insert", map[string]any{
		"namespace": "fid", "table": "nums",
		"records": []map[string]any{{"scalar": json.Number("1e400")}},
	})
	wErrObj, _ := wBody["error"].(map[string]any)
	if wStatus != 400 || wErrObj["code"] != "invalid_request" {
		t.Fatalf("an out-of-float64-range number over HTTP must be rejected 400 invalid_request, got %d %v", wStatus, wBody)
	}
	if _, err := st.Insert(ctx, "fid", "nums", []map[string]any{{"scalar": json.Number("1e400")}}, dolmen.InsertOptions{}); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("an out-of-float64-range json.Number must be rejected invalid_request, got %v", err)
	}
}

func blobNumber(t *testing.T, blob any, key string) (string, bool) {
	t.Helper()
	m, ok := blob.(map[string]any)
	if !ok {
		return "", false
	}
	n, ok := m[key].(json.Number)
	if !ok {
		t.Fatalf("blob field %q must decode as a numeric json.Number token, got %T — a quoted-string regression on either surface must fail here, not pass on matching digits", key, m[key])
	}
	return n.String(), true
}

func numberKeyOf(t *testing.T, v any) string {
	t.Helper()
	k, ok := numberKey(canonical(v))
	if !ok {
		t.Fatalf("numberKeyOf: %v (%T) is not a number", v, v)
	}
	return k
}

func TestParityTerminalPageNotTruncated(t *testing.T) {
	h := newHarness(t)
	seedParityTableHTTP(t, h)
	h.mustHTTPNumbered("insert", map[string]any{"namespace": "par", "table": "notes", "records": parityRecords[:2]})
	httpSecond := h.mustHTTPNumbered("query", map[string]any{
		"namespace": "par", "sql": "SELECT id, body, score FROM notes WHERE score >= ? ORDER BY score", "args": []any{1}, "limit": 1, "offset": 1,
	})
	if httpSecond["truncated"] != false {
		t.Fatalf("the terminal page (offset past the last full page) must report truncated=false, got %v", httpSecond["truncated"])
	}

	st, _ := embeddedStore(t)
	seedParityTableEmbedded(t, st)
	ctx := context.Background()
	if _, err := st.Insert(ctx, "par", "notes", parityRecords[:2], dolmen.InsertOptions{}); err != nil {
		t.Fatalf("embedded insert: %v", err)
	}
	embSecond, err := st.Query(ctx, "par", "SELECT id, body, score FROM notes WHERE score >= ? ORDER BY score", dolmen.QueryOptions{Args: []any{1}, Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("embedded query: %v", err)
	}
	if embSecond.Truncated {
		t.Fatalf("the terminal page must not report truncated on the embedded surface either")
	}
}

func TestParityDefaultsMaterializeOnBothSurfaces(t *testing.T) {
	h := newHarness(t)
	seedParityTableHTTP(t, h)
	httpRes := h.mustHTTPNumbered("insert", map[string]any{
		"namespace": "par", "table": "notes", "records": []map[string]any{parityRecords[2]},
	})
	httpRows := h.mustHTTPNumbered("read_rows", map[string]any{
		"namespace": "par", "table": "notes", "ids": httpRes["ids"],
	})
	hRow := httpRows["rows"].([]any)[0].(map[string]any)
	rank, ok := hRow["rank"]
	if !ok {
		t.Fatalf("the wire must MATERIALIZE the omitted default (rank), got fields %v", hRow)
	}
	if k := numberKeyOf(t, rank); k != "7" {
		t.Fatalf("wire default rank must be 7, got %q", k)
	}

	st, _ := embeddedStore(t)
	seedParityTableEmbedded(t, st)
	ctx := context.Background()
	ins, err := st.Insert(ctx, "par", "notes", []map[string]any{parityRecords[2]}, dolmen.InsertOptions{})
	if err != nil {
		t.Fatalf("embedded insert: %v", err)
	}
	embRows, err := st.GetRows(ctx, "par", "notes", ins.Ids)
	if err != nil {
		t.Fatalf("embedded read: %v", err)
	}
	erank, ok := embRows.Rows[0]["rank"]
	if !ok {
		t.Fatalf("the embedded surface must MATERIALIZE the omitted default (rank), got fields %v", embRows.Rows[0])
	}
	if k := numberKeyOf(t, erank); k != "7" {
		t.Fatalf("embedded default rank must be 7, got %q", k)
	}
}

func TestEmbeddedParityReadsNeverCreateNamespaces(t *testing.T) {
	dir := t.TempDir()
	emb := &parityProvider{}
	st := openEmbedded(t, dir, dolmen.WithEmbedding(emb))
	defer st.Close()
	ctx := context.Background()

	reads := map[string]func(ns string) error{
		"ListTables": func(ns string) error {
			_, err := st.ListTables(ctx, ns)
			return err
		},
		"DescribeTable": func(ns string) error {
			_, _, err := st.DescribeTable(ctx, ns, "t")
			return err
		},
		"GetRows": func(ns string) error {
			_, err := st.GetRows(ctx, ns, "t", []int64{1})
			return err
		},
		"Query": func(ns string) error {
			_, err := st.Query(ctx, ns, "SELECT 1", dolmen.QueryOptions{})
			return err
		},
		"SearchFulltext": func(ns string) error {
			_, err := st.SearchFulltext(ctx, ns, "t", "x", dolmen.SearchOptions{})
			return err
		},
		"SearchVectorRaw": func(ns string) error {
			_, err := st.SearchVector(ctx, ns, "t", dolmen.VectorQuery{Vec: []float32{1, 2, 3, 4, 5, 6, 7, 8}}, dolmen.SearchOptions{})
			return err
		},
		"SearchVectorText": func(ns string) error {
			_, err := st.SearchVector(ctx, ns, "t", dolmen.VectorQuery{Text: "x"}, dolmen.SearchOptions{})
			return err
		},
		"DropTable": func(ns string) error {
			return st.DropTable(ctx, ns, "t")
		},
	}

	names := make([]string, 0, len(reads))
	for name := range reads {
		names = append(names, name)
	}
	sort.Strings(names)

	for i, name := range names {
		ns := "facadeghost" + strconv.Itoa(i)
		if err := reads[name](ns); !errors.Is(err, dolmen.ErrNotFound) {
			t.Fatalf("facade %s on a missing namespace = %v, want ErrNotFound — the wire surface answers not_found", name, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("a facade read against a missing namespace left %s on disk; the facade must match the wire surface", e.Name())
	}
}

func TestEmbeddedParityWritesStillCreateNamespaces(t *testing.T) {
	dir := t.TempDir()
	st := openEmbedded(t, dir, dolmen.WithEmbedding(&parityProvider{}))
	defer st.Close()
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "facadeborn", "notes", []dolmen.Field{{Name: "body", Type: dolmen.Text}}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Run("sqlite namespace file appears on write", func(t *testing.T) {
		sqliteOnly(t)
		if _, err := os.Stat(filepath.Join(dir, "facadeborn.db")); err != nil {
			t.Fatalf("a facade write must create its namespace implicitly: %v", err)
		}
	})
	tables, err := st.ListTables(ctx, "facadeborn")
	if err != nil || len(tables) != 1 || tables[0] != "notes" {
		t.Fatalf("the created table must be listed, got %v (%v)", tables, err)
	}
}
