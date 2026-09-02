package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

type fakeEmb struct{}

func (fakeEmb) Name() string { return "fake" }

func (fakeEmb) Identity() string { return "fake-space" }

func (fakeEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
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

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, fakeEmb{}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, base, op string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := http.Post(base+"/v1/"+op, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", op, err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode %s response: %v", op, err)
	}
	return res.StatusCode, decoded
}

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

	res2, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != 200 {
		t.Fatalf("healthz status %d", res2.StatusCode)
	}
}

func TestInferSchemaEndpoint(t *testing.T) {
	srv := newTestServer(t)
	long := fmt.Sprintf("A detailed finding body.%s", bytes.Repeat([]byte(" x"), 150))

	code, res := post(t, srv.URL, "infer_schema", map[string]any{
		"samples": []map[string]any{
			{"title": "bug", "score": 3.5, "ok": true, "when": "2026-09-01T10:00:00Z", "detail": long, "tags": []any{"a"}},
		},
	})
	if code != 200 {
		t.Fatalf("infer_schema failed: %d %v", code, res)
	}
	fields := res["data"].(map[string]any)["fields"].([]any)
	byName := map[string]map[string]any{}
	for _, f := range fields {
		m := f.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if byName["score"]["type"] != "number" ||
		byName["ok"]["type"] != "boolean" ||
		byName["when"]["type"] != "timestamp" ||
		byName["tags"]["type"] != "json" {
		t.Fatalf("inferred types wrong: %v", byName)
	}
	if byName["detail"]["type"] != "text" || byName["detail"]["fulltext"] != true {
		t.Fatalf("long text should be text+fulltext: %v", byName["detail"])
	}
}

func TestOriginGuard(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := httptest.NewServer(OriginGuard(New(st, fakeEmb{}).Handler(), []string{"https://app.example.com"}))
	t.Cleanup(srv.Close)

	do := func(origin, contentType string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/list_tables", bytes.NewReader([]byte(`{"namespace":"x"}`)))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if code := do("http://evil.example", "application/json"); code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation must be rejected, got %d", code)
	}
	if code := do("http://localhost:5173", "application/json"); code != http.StatusOK {
		t.Fatalf("localhost origin must pass, got %d", code)
	}
	if code := do("", "application/json"); code != http.StatusOK {
		t.Fatalf("no-origin (curl/server) must pass, got %d", code)
	}
	if code := do("https://app.example.com", "application/json"); code != http.StatusOK {
		t.Fatalf("allowlisted origin must pass, got %d", code)
	}
	if code := do("", "text/plain"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON content type must be rejected, got %d", code)
	}

	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET healthz must pass guard, got %d", res.StatusCode)
	}
}

func TestCORSPreflight(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := httptest.NewServer(OriginGuard(New(st, fakeEmb{}).Handler(), []string{"https://app.example.com"}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/insert", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("preflight for allowed origin failed: %d %q", res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
	}

	req, _ = http.NewRequest(http.MethodOptions, srv.URL+"/v1/insert", nil)
	req.Header.Set("Origin", "http://evil.example")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("preflight for disallowed origin must be 403, got %d", res.StatusCode)
	}
}
