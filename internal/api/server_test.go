package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestTrailingContentRejected(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Post(srv.URL+"/v1/list_tables", "application/json",
		strings.NewReader(`{"namespace":"x"} {"namespace":"y"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for trailing content, got %d", res.StatusCode)
	}
}

func TestListTablesEmptyNamespaceReturnsArray(t *testing.T) {
	srv := newTestServer(t)
	code, body := post(t, srv.URL, "list_tables", map[string]any{"namespace": "fresh"})
	if code != http.StatusOK {
		t.Fatalf("list_tables on fresh namespace: %d %v", code, body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %v", body)
	}
	tables, ok := data["tables"].([]any)
	if !ok {
		t.Fatalf(`"tables" must serialize as an array, got %T (%v)`, data["tables"], data["tables"])
	}
	if len(tables) != 0 {
		t.Fatalf("fresh namespace must list zero tables, got %v", tables)
	}
}

func TestCreateTableDimSchemaIsInteger(t *testing.T) {
	def, ok := Ops["create_table"]
	if !ok {
		t.Fatal("create_table op missing")
	}
	fields, ok := def.InputSchema["properties"].(map[string]any)["fields"].(map[string]any)
	if !ok {
		t.Fatal("fields property missing")
	}
	items := fields["items"].(map[string]any)
	dim := items["properties"].(map[string]any)["dim"].(map[string]any)
	if dim["type"] != "integer" {
		t.Fatalf(`"dim" must be declared integer (it decodes into an int field), got %v`, dim["type"])
	}
}

func TestInferSchemaEmptyObjectsReturnArray(t *testing.T) {
	srv := newTestServer(t)
	code, body := post(t, srv.URL, "infer_schema", map[string]any{"samples": []map[string]any{{}}})
	if code != http.StatusOK {
		t.Fatalf("infer_schema with empty objects: %d %v", code, body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %v", body)
	}
	fields, ok := data["fields"].([]any)
	if !ok {
		t.Fatalf(`"fields" must serialize as an array, got %T (%v)`, data["fields"], data["fields"])
	}
	if len(fields) != 0 {
		t.Fatalf("empty samples must infer zero fields, got %v", fields)
	}
}

func TestOversizedBodyReturns413(t *testing.T) {
	srv := newTestServer(t)
	big := bytes.Repeat([]byte("a"), 33<<20)
	res, err := http.Post(srv.URL+"/v1/list_tables", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body must return 413, got %d", res.StatusCode)
	}
}

func TestContentTypeParsedExactly(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(OriginGuard(New(st, fakeEmb{}).Handler(), nil))
	t.Cleanup(srv.Close)
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "APPLICATION/JSON"} {
		res, err := http.Post(srv.URL+"/v1/list_tables", ct, strings.NewReader(`{"namespace":"x"}`))
		if err != nil {
			t.Fatalf("post %s: %v", ct, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("valid media type %q must be accepted, got %d", ct, res.StatusCode)
		}
	}
	for _, ct := range []string{"application/jsonp", "application/json-foo", "text/plain"} {
		res, err := http.Post(srv.URL+"/v1/list_tables", ct, strings.NewReader(`{"namespace":"x"}`))
		if err != nil {
			t.Fatalf("post %s: %v", ct, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("invalid media type %q must be rejected with 415, got %d", ct, res.StatusCode)
		}
	}
}
