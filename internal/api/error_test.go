package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func errorBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope object, got %T", body["error"])
	}
	return errObj
}

func TestErrorEnvelopeHasStableCodeAndMessage(t *testing.T) {
	srv := newTestServer(t)

	code, body := post(t, srv.URL, "query", map[string]any{
		"namespace": "x",
		"sql":       "SELECT * FROOOM notes",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %v", code, body)
	}
	errObj := errorBody(t, body)
	if errObj["code"] != "query_error" {
		t.Fatalf("expected query_error code, got %v", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if msg == "" {
		t.Fatalf("expected a message, got %v", errObj)
	}
	if strings.Contains(msg, "SQL logic error") || strings.Contains(msg, "(1)") {
		t.Fatalf("raw SQLite internals leaked into message: %q", msg)
	}
	if !strings.Contains(msg, "SELECT or WITH") {
		t.Fatalf("expected guidance about allowed syntax, got %q", msg)
	}
	if !strings.Contains(msg, "near") {
		t.Fatalf("expected the offending token to be named, got %q", msg)
	}
}

func TestErrorEnvelopeEchoesRequestID(t *testing.T) {
	srv := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/query", strings.NewReader(`{"namespace":"x","sql":"SELECT ("}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "req-42")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()

	if res.Header.Get("X-Request-Id") != "req-42" {
		t.Fatalf("expected X-Request-Id header echoed, got %q", res.Header.Get("X-Request-Id"))
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj := errorBody(t, body)
	if errObj["request_id"] != "req-42" {
		t.Fatalf("expected request_id echoed, got %v", errObj["request_id"])
	}
}

func TestErrorEnvelopeNotFoundForMissingTable(t *testing.T) {
	srv := newTestServer(t)

	code, body := post(t, srv.URL, "query", map[string]any{
		"namespace": "x",
		"sql":       "SELECT * FROM missing",
	})
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing table, got %d %v", code, body)
	}
	errObj := errorBody(t, body)
	if errObj["code"] != "not_found" {
		t.Fatalf("expected not_found code, got %v", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "missing") {
		t.Fatalf("expected table name in message, got %q", msg)
	}
	if strings.Contains(msg, "SQL logic error") {
		t.Fatalf("raw SQLite leaked into message: %q", msg)
	}
}

func TestErrorEnvelopeRedactsStoreInternalErrors(t *testing.T) {
	srv := newTestServer(t)

	// create_table with a reserved table name triggers an ErrInvalid. The
	// envelope must expose a clean message and code, not raw details.
	code, body := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "x",
		"table":     "id",
		"fields":    []map[string]any{{"name": "email", "type": "string"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %v", code, body)
	}
	errObj := errorBody(t, body)
	if errObj["code"] != "invalid_request" {
		t.Fatalf("expected invalid_request code, got %v", errObj["code"])
	}
}

func TestErrorEnvelopeConflict(t *testing.T) {
	srv := newTestServer(t)
	mustCreateUsers(t, srv.URL)

	body := map[string]any{
		"namespace": "app", "table": "users",
		"records":         []map[string]any{{"email": "a@example.com", "plan": "free"}},
		"idempotency_key": "retry-safe-1",
	}
	code, res := post(t, srv.URL, "insert", body)
	if code != 200 || res["ok"] != true {
		t.Fatalf("first insert failed: %d %v", code, res)
	}

	body["records"] = []map[string]any{{"email": "b@example.com", "plan": "pro"}}
	code, res = post(t, srv.URL, "insert", body)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %v", code, res)
	}
	errObj := errorBody(t, res)
	if errObj["code"] != "conflict" {
		t.Fatalf("expected conflict code, got %v", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "idempotency key") {
		t.Fatalf("expected idempotency message, got %q", msg)
	}
}

func TestErrorEnvelopeSanitizesBodyReadErrors(t *testing.T) {
	srv := newTestServer(t)
	big := bytes.Repeat([]byte("a"), 33<<20)
	res, err := http.Post(srv.URL+"/v1/list_tables", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj := errorBody(t, body)
	if errObj["code"] != "invalid_request" {
		t.Fatalf("expected invalid_request code for oversized body, got %v", errObj["code"])
	}
}

func TestRedactStoreMsgRedactsFilePaths(t *testing.T) {
	err := fmt.Errorf("%w: open /tmp/secret: no such file", store.ErrInvalid)
	apiErr := wrapStoreErr(err)
	if apiErr == nil {
		t.Fatal("expected wrapped error")
	}
	if !strings.Contains(apiErr.Message, "<path>") {
		t.Fatalf("expected path redaction, got %q", apiErr.Message)
	}
	if strings.Contains(apiErr.Message, "/tmp/secret") {
		t.Fatalf("path leaked: %q", apiErr.Message)
	}
}

func TestRedactStoreMsgPreservesProviderIdentity(t *testing.T) {
	a := "openai|https://api.openai.com/v1|text-embedding-3-small"
	b := "openai|https://proxy.example.com/v1|text-embedding-3-small"
	err := fmt.Errorf("%w: embedding provider changed: table rows were embedded by %q but the active provider is %q", store.ErrInvalid, a, b)
	apiErr := wrapStoreErr(err)
	if apiErr == nil {
		t.Fatal("expected wrapped error")
	}
	if strings.Contains(apiErr.Message, "<path>") {
		t.Fatalf("redaction collapsed provider identities: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "https://api.openai.com/v1") || !strings.Contains(apiErr.Message, "https://proxy.example.com/v1") {
		t.Fatalf("provider URLs must remain distinguishable: %q", apiErr.Message)
	}
}

func TestErrorEnvelopeOmitsRequestIDWhenNotProvided(t *testing.T) {
	srv := newTestServer(t)
	code, body := post(t, srv.URL, "query", map[string]any{
		"namespace": "x",
		"sql":       "SELECT (",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %v", code, body)
	}
	errObj := errorBody(t, body)
	if _, ok := errObj["request_id"]; ok {
		t.Fatalf("request_id should be omitted when not provided, got %v", errObj)
	}
}

