package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/store"
)

// localStub returns a Local provider whose engine is swapped for a stub, so
// the full create/insert/search HTTP path runs against the real provider
// wiring (identity, lazy load) without a model download.
func localStub(model string, dim int) *embed.Local {
	return &embed.Local{
		Model: model,
		Open: func() (embed.LocalEngine, error) {
			return stubEngine{dim: dim}, nil
		},
	}
}

type stubEngine struct{ dim int }

func (s stubEngine) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, s.dim)
		for j := range out[i] {
			out[i][j] = 0.1
		}
	}
	return out, nil
}

// TestLocalProviderVectorizeEndToEnd pins the acceptance path of the local
// provider: a server with no external endpoint creates a vectorized table,
// inserts rows (embedding through the provider seam), and answers a text
// search against the server-managed _embedding space.
func TestLocalProviderVectorizeEndToEnd(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, localStub("sentence-transformers/all-MiniLM-L6-v2", 4)).Handler())
	t.Cleanup(srv.Close)

	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "app",
		"table":     "docs",
		"fields":    []map[string]any{{"name": "body", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatalf("create_table with vectorize on a local-provider server: %d %v", code, res)
	}
	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace": "app",
		"table":     "docs",
		"records":   []map[string]any{{"body": "first"}, {"body": "second"}},
	})
	if code != 200 {
		t.Fatalf("insert: %d %v", code, res)
	}
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "app", "table": "docs", "text": "first",
	})
	if code != 200 {
		t.Fatalf("search_vector text: %d %v", code, res)
	}
	results, _ := res["data"].(map[string]any)["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("text search must return rows, got %v", res)
	}

	// The table's embedding space is pinned to local/<model>.
	code, res = post(t, srv.URL, "describe_table", map[string]any{"namespace": "app", "table": "docs"})
	if code != 200 {
		t.Fatalf("describe_table: %d %v", code, res)
	}
	table, _ := res["data"].(map[string]any)["table"].(map[string]any)
	if got := table["embed_space"]; got != "local/sentence-transformers/all-MiniLM-L6-v2" {
		t.Fatalf("embed_space must pin the local provider identity, got %v", got)
	}
}

// TestLocalProviderSwitchRejected pins provider-switch rejection parity: a
// table embedded by local/<model-a> refuses inserts from local/<model-b>
// until re-embedded via migrate, exactly as with the OpenAI provider.
func TestLocalProviderSwitchRejected(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srvA := httptest.NewServer(New(st, localStub("org/model-a", 4)).Handler())
	t.Cleanup(srvA.Close)

	code, res := post(t, srvA.URL, "create_table", map[string]any{
		"namespace": "app",
		"table":     "docs",
		"fields":    []map[string]any{{"name": "body", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatalf("create_table: %d %v", code, res)
	}
	code, res = post(t, srvA.URL, "insert", map[string]any{
		"namespace": "app", "table": "docs", "records": []map[string]any{{"body": "hello"}},
	})
	if code != 200 {
		t.Fatalf("insert under model-a: %d %v", code, res)
	}

	// Same store, server restarted with a different local model.
	srvB := httptest.NewServer(New(st, localStub("org/model-b", 4)).Handler())
	t.Cleanup(srvB.Close)
	code, res = post(t, srvB.URL, "insert", map[string]any{
		"namespace": "app", "table": "docs", "records": []map[string]any{{"body": "more"}},
	})
	if code != 400 {
		t.Fatalf("insert under a different local model must be rejected, got %d %v", code, res)
	}
	if msg, _ := res["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "embedding provider changed") {
		t.Fatalf("rejection must explain the provider change, got %v", res)
	}
	code, res = post(t, srvB.URL, "search_vector", map[string]any{
		"namespace": "app", "table": "docs", "text": "hello",
	})
	if code != 400 {
		t.Fatalf("text search under a different local model must be rejected, got %d %v", code, res)
	}

	// The re-embed flow clears the pin: set_vectorize off, then on.
	vecOff := map[string]any{"op": "set_vectorize", "name": "body", "value": false}
	code, _ = post(t, srvB.URL, "migrate", map[string]any{
		"namespace": "app", "table": "docs",
		"changes": []map[string]any{vecOff},
	})
	if code != 200 {
		t.Fatalf("migrate vectorize off: %d", code)
	}
	vecOn := map[string]any{"op": "set_vectorize", "name": "body", "value": true}
	code, res = post(t, srvB.URL, "migrate", map[string]any{
		"namespace": "app", "table": "docs",
		"changes": []map[string]any{vecOn},
	})
	if code != 200 {
		t.Fatalf("migrate vectorize on under model-b: %d %v", code, res)
	}
	code, res = post(t, srvB.URL, "insert", map[string]any{
		"namespace": "app", "table": "docs", "records": []map[string]any{{"body": "more"}},
	})
	if code != 200 {
		t.Fatalf("insert after re-embed under model-b: %d %v", code, res)
	}
}

// recordingEngine captures the texts the provider is asked to embed, so the
// e5 prefix test can assert what reached the engine on both the insert
// (passage) and search (query) paths.
type recordingEngine struct {
	mu    sync.Mutex
	texts []string
	dim   int
}

func (r *recordingEngine) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.texts = append(r.texts, texts...)
	r.mu.Unlock()
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, r.dim)
		for j := range out[i] {
			out[i][j] = 0.1
		}
	}
	return out, nil
}

func (r *recordingEngine) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.texts...)
}

// TestLocalProviderE5PrefixesEndToEnd pins the e5 role-prefix contract over
// HTTP: with an e5-family model configured, inserts embed "passage: <text>"
// and search_vector embeds "query: <text>". Dolmen adds the prefixes
// server-side — callers never see or supply them.
func TestLocalProviderE5PrefixesEndToEnd(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	eng := &recordingEngine{dim: 4}
	srv := httptest.NewServer(New(st, &embed.Local{
		Model: "intfloat/multilingual-e5-small",
		Open:  func() (embed.LocalEngine, error) { return eng, nil },
	}).Handler())
	t.Cleanup(srv.Close)

	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "e5",
		"table":     "incidents",
		"fields":    []map[string]any{{"name": "body", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatalf("create_table: %d %v", code, res)
	}
	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace": "e5", "table": "incidents",
		"records": []map[string]any{{"body": "接続プール枯渏"}},
	})
	if code != 200 {
		t.Fatalf("insert: %d %v", code, res)
	}
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "e5", "table": "incidents", "text": "connection pool exhausted",
	})
	if code != 200 {
		t.Fatalf("search_vector text: %d %v", code, res)
	}

	texts := eng.recorded()
	wantPassage, wantQuery := "passage: 接続プール枯渏", "query: connection pool exhausted"
	var gotPassage, gotQuery bool
	for _, tx := range texts {
		if tx == wantPassage {
			gotPassage = true
		}
		if tx == wantQuery {
			gotQuery = true
		}
	}
	if !gotPassage {
		t.Errorf("engine must receive %q for the stored row, got %q", wantPassage, texts)
	}
	if !gotQuery {
		t.Errorf("engine must receive %q for the search text, got %q", wantQuery, texts)
	}

	// The table's embedding space carries the #e5 marker — the prefix
	// contract is part of the pinned identity.
	code, res = post(t, srv.URL, "describe_table", map[string]any{"namespace": "e5", "table": "incidents"})
	if code != 200 {
		t.Fatalf("describe_table: %d %v", code, res)
	}
	table, _ := res["data"].(map[string]any)["table"].(map[string]any)
	if got := table["embed_space"]; got != "local/v2:intfloat/multilingual-e5-small#e5" {
		t.Fatalf("embed_space must carry the versioned #e5 marker, got %v", got)
	}
}

// TestLocalProviderLoadFailureActionable pins the #144 contract: a first-use
// local-model download failure surfaces as an actionable embedder_unavailable
// error whose message names the offline remediations — never a bare
// internal_error — with a request id in the envelope, the response header,
// and the server log. The failed insert lands no rows and burns no
// idempotency key, exactly as the round-3 black-box run verified.
func TestLocalProviderLoadFailureActionable(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// The stub's error mimics a blocked Hugging Face download whose text a
	// naive message would leak: a hub URL, a cache path, and a cache path
	// under a space-bearing directory. None of it may reach the client — the
	// cause belongs to the server log, correlated by request id.
	blockedDownload := errors.New(`Get "https://huggingface.co/org/model/resolve/main/config.json": TLS handshake timeout (cache dir ` + filepath.Join(t.TempDir(), "models", "org--model") + `; also tried C:\Users\Jane Doe\models\org--model)`)
	failing := &embed.Local{
		Model: "org/model",
		Open: func() (embed.LocalEngine, error) {
			return nil, blockedDownload
		},
	}
	srv := httptest.NewServer(New(st, failing).Handler())
	t.Cleanup(srv.Close)

	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "app",
		"table":     "docs",
		"fields":    []map[string]any{{"name": "body", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatalf("create_table with vectorize must succeed (the load is lazy): %d %v", code, res)
	}

	// Capture the server log around the failing insert; the request carries
	// no X-Request-Id, so the id must be server-generated.
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	insertBody := `{"namespace":"app","table":"docs","idempotency_key":"retry-1","records":[{"body":"one"},{"body":"two"},{"body":"three"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/insert", strings.NewReader(insertBody))
	req.Header.Set("Content-Type", "application/json")
	insertRes, err := http.DefaultClient.Do(req)
	slog.SetDefault(oldLogger)
	if err != nil {
		t.Fatalf("post insert: %v", err)
	}
	defer insertRes.Body.Close()
	if insertRes.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("blocked-download insert must be 503, got %d", insertRes.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(insertRes.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj := errorBody(t, body)
	if errObj["code"] != "embedder_unavailable" {
		t.Fatalf("code %v, want embedder_unavailable: %v", errObj["code"], errObj)
	}
	msg, _ := errObj["message"].(string)
	for _, want := range []string{
		"org/model",                // which model failed
		"pre-seed the model cache", // remediation 1 (README local provider notes)
		"DOLMEN_EMBED_MODEL",       // remediation 2 (absolute model-directory path)
		"model-directory path",
		"Hugging Face Hub",
		"this request id", // points at the log correlation
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message must name %q for the operator, got %q", want, msg)
		}
	}
	// The raw downloader cause never reaches the client: it can carry signed
	// CDN URLs, proxy credentials, or internal endpoints, which no redaction
	// of arbitrary text can promise removed. It belongs to the server log.
	for _, leak := range []string{
		"TLS handshake timeout",            // the cause text itself
		"huggingface.co/org/model/resolve", // a URL from the cause
		"models/org--model",                // cache paths
		"Jane", "Doe",                      // space-bearing path fragments
	} {
		if strings.Contains(msg, leak) {
			t.Fatalf("downloader cause %q leaked into the client message, got %q", leak, msg)
		}
	}

	reqID, _ := errObj["request_id"].(string)
	if reqID == "" {
		t.Fatalf("envelope must carry a request_id even when the client sent none, got %v", errObj)
	}
	if got := insertRes.Header.Get("X-Request-Id"); got != reqID {
		t.Fatalf("X-Request-Id header %q must match envelope request_id %q", got, reqID)
	}
	logs := logBuf.String()
	for _, want := range []string{
		"level=ERROR", // server-class failure, visible at Info
		"code=embedder_unavailable",
		"status=503",
		"request_id=" + reqID,   // the log line carries the same id
		"TLS handshake timeout", // the cause reaches the log
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("server log must contain %q, got:\n%s", want, logs)
		}
	}

	// Text queries embed server-side, so the same blocked load classifies the
	// same way on the search path.
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "app", "table": "docs", "text": "anything",
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("text search under a blocked download must be 503, got %d %v", code, res)
	}
	if got := errorBody(t, res)["code"]; got != "embedder_unavailable" {
		t.Fatalf("search code %v, want embedder_unavailable", got)
	}

	// The failed insert rolled back atomically: no rows landed.
	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "app", "sql": "SELECT COUNT(*) AS n FROM docs",
	})
	if code != 200 {
		t.Fatalf("count query: %d %v", code, res)
	}
	rows, _ := res["data"].(map[string]any)["rows"].([]any)
	if n := rows[0].(map[string]any)["n"].(float64); n != 0 {
		t.Fatalf("blocked insert must leave the table empty, got %d rows", int64(n))
	}

	// The idempotency key was not burned: after the operator fixes the load
	// (here: a server whose engine loads), the same key with the same
	// records inserts fresh instead of failing as key reuse.
	fixed := httptest.NewServer(New(st, localStub("org/model", 4)).Handler())
	t.Cleanup(fixed.Close)
	code, res = post(t, fixed.URL, "insert", map[string]any{
		"namespace": "app", "table": "docs", "idempotency_key": "retry-1",
		"records": []map[string]any{{"body": "one"}, {"body": "two"}, {"body": "three"}},
	})
	if code != 200 {
		t.Fatalf("retry after the load is fixed must succeed (key not burned): %d %v", code, res)
	}
	data, _ := res["data"].(map[string]any)
	if data["inserted"] != float64(3) || data["replayed"] != false {
		t.Fatalf("retry must be a fresh insert (inserted=3, replayed=false), got %v", data)
	}
}
