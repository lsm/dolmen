package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/embed"
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
		"records":         []map[string]any{{"email": "a@example.com", "plan_name": "free"}},
		"idempotency_key": "retry-safe-1",
	}
	code, res := post(t, srv.URL, "insert", body)
	if code != 200 || res["ok"] != true {
		t.Fatalf("first insert failed: %d %v", code, res)
	}

	body["records"] = []map[string]any{{"email": "b@example.com", "plan_name": "pro"}}
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

// TestRedactPathsCoversSpacesInPaths pins the redaction of paths whose
// components contain spaces — a model cache under a user directory
// ("C:\Users\Jane Doe\models") must redact whole, never leaking a trailing
// fragment like "Doe\models" into a client-facing message.
func TestRedactPathsCoversSpacesInPaths(t *testing.T) {
	cases := []string{
		`open C:\Users\Jane Doe\models\org--model: not found`,
		`open /Users/Jane Doe/models/org--model: not found`,
		`cache /a/b  spaced dir/x is missing`, // multiple spaces between words
	}
	for _, msg := range cases {
		out := redactPaths(msg)
		if strings.Contains(out, "Jane") || strings.Contains(out, "Doe") || strings.Contains(out, "models") || strings.Contains(out, "spaced") {
			t.Fatalf("path fragment leaked from %q: %q", msg, out)
		}
		if !strings.Contains(out, "<path>") {
			t.Fatalf("expected <path> in redaction of %q, got %q", msg, out)
		}
	}
}

// TestEmbedderUnavailableMessageNamesModelSafely pins what the public
// embedder_unavailable message may name: a Hub id (org/name — a public
// identifier) is echoed, but a model-directory path is filesystem layout and
// is never interpolated, ASCII or not.
func TestEmbedderUnavailableMessageNamesModelSafely(t *testing.T) {
	hub := wrapStoreErr(&embed.LoadError{Model: "org/model", Err: errors.New("download failed")})
	if hub.Code != ErrCodeEmbedderUnavailable || hub.Status != http.StatusServiceUnavailable {
		t.Fatalf("code/status: %v %d", hub.Code, hub.Status)
	}
	if !strings.Contains(hub.Message, "org/model") || !strings.Contains(hub.Message, "Hugging Face Hub") {
		t.Fatalf("Hub-id model must be named, got %q", hub.Message)
	}
	if strings.Contains(hub.Message, "download failed") {
		t.Fatalf("raw cause must stay out of the public message, got %q", hub.Message)
	}

	dir := wrapStoreErr(&embed.LoadError{Model: "/Users/张三/models", Err: errors.New("download failed")})
	for _, leak := range []string{"张三", "/Users", "models"} {
		if strings.Contains(dir.Message, leak) {
			t.Fatalf("directory model leaked %q into the public message, got %q", leak, dir.Message)
		}
	}
	if !strings.Contains(dir.Message, "configured local model directory") {
		t.Fatalf("directory model must be described generically, got %q", dir.Message)
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

func TestErrorEnvelopeQueryErrorForMalformedFilter(t *testing.T) {
	srv := newTestServer(t)
	mustCreateUsers(t, srv.URL)

	code, body := post(t, srv.URL, "update", map[string]any{
		"namespace": "app",
		"table":     "users",
		"filter":    "id =",
		"set":       map[string]any{"plan_name": "pro"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %v", code, body)
	}
	errObj := errorBody(t, body)
	if errObj["code"] != "query_error" {
		t.Fatalf("expected query_error code, got %v", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if strings.Contains(msg, "SQL logic error") || strings.Contains(msg, "(1)") {
		t.Fatalf("raw SQLite internals leaked into message: %q", msg)
	}
	if !strings.Contains(msg, "WHERE expression") {
		t.Fatalf("filter failures must carry WHERE-expression guidance, got %q", msg)
	}
	if strings.Contains(msg, "SELECT or WITH") {
		t.Fatalf("filter guidance must not point at SELECT/WITH statements, got %q", msg)
	}

	code, body = post(t, srv.URL, "delete", map[string]any{
		"namespace": "app",
		"table":     "users",
		"filter":    "id =",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %v", code, body)
	}
	errObj = errorBody(t, body)
	if errObj["code"] != "query_error" {
		t.Fatalf("expected query_error code for delete, got %v", errObj["code"])
	}
	msg, _ = errObj["message"].(string)
	if !strings.Contains(msg, "WHERE expression") {
		t.Fatalf("delete filter failures must carry WHERE-expression guidance, got %q", msg)
	}

	// Vector search filter failures must also classify as query errors.
	code, _ = post(t, srv.URL, "create_table", map[string]any{
		"namespace": "app",
		"table":     "vectors",
		"fields":    []map[string]any{{"name": "text", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatalf("create_table for vector search failed: %d", code)
	}
	code, body = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "app",
		"table":     "vectors",
		"vector":    []float64{1, 0, 0, 0, 0, 0, 0, 0},
		"filter":    "id =",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %v", code, body)
	}
	errObj = errorBody(t, body)
	if errObj["code"] != "query_error" {
		t.Fatalf("expected query_error code for search_vector filter, got %v", errObj["code"])
	}
	msg, _ = errObj["message"].(string)
	if strings.Contains(msg, "SQL logic error") || strings.Contains(msg, "(1)") {
		t.Fatalf("raw SQLite internals leaked into message: %q", msg)
	}
}

// TestErrorEnvelopeGeneratesRequestIDWhenNotProvided pins the always-on
// request-id contract (#144): a client that sends no X-Request-Id still gets
// one — server-generated, in the envelope and the response header — so any
// failure can be correlated with the server log.
func TestErrorEnvelopeGeneratesRequestIDWhenNotProvided(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/query", strings.NewReader(`{"namespace":"x","sql":"SELECT ("}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj := errorBody(t, body)
	reqID, _ := errObj["request_id"].(string)
	if reqID == "" {
		t.Fatalf("request_id must be generated when the client sent none, got %v", errObj)
	}
	if got := res.Header.Get("X-Request-Id"); got != reqID {
		t.Fatalf("X-Request-Id header %q must match envelope request_id %q", got, reqID)
	}
}
