package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/mcp"
	"github.com/lsm/dolmen/internal/store"
)

// fakeProvider is a deterministic in-process embedding provider. It counts
// calls (dry_run purity, re-embed assertions) and can be armed to fail so a
// provider outage is observable through the transports. Vectors are a stable
// function of the text, so the same text always embeds to the same vector.
type fakeProvider struct {
	mu    sync.Mutex
	calls int
	texts []string
	fail  error
}

func (p *fakeProvider) Name() string      { return "conformance" }
func (p *fakeProvider) Identity() string  { return "conformance|fake|v1" }
func (p *fakeProvider) ModelName() string { return "fake-model" }

func (p *fakeProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	p.mu.Lock()
	p.calls++
	p.texts = append(p.texts, texts...)
	fail := p.fail
	p.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		for j, r := range []byte(t) {
			v[r%8] += float32(j + 1)
		}
		// Guarantee a nonzero norm so cosine is well defined for any text.
		if t == "" {
			v[0] = 1
		}
		out[i] = v
	}
	return out, nil
}

// EmbedQuery tracks query-side calls like Embed tracks passage-side ones.
func (p *fakeProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := p.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *fakeProvider) embeddedTexts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.texts...)
}

// harness is one dolmen server exposed over both transports, wired exactly
// like main.go: OriginGuard over a mux with /mcp on the MCP server and / on
// the API handler. It keeps the data directory so tests can reopen the store
// (durability) and reach into the namespace file (out-of-band fixtures).
type harness struct {
	t   *testing.T
	dir string
	srv *httptest.Server
	st  *store.Store
	emb *fakeProvider
	auth *api.Auth

	httpURL string // .../v1
	mcpURL  string // .../mcp
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessAt(t, t.TempDir(), &fakeProvider{})
}

func newHarnessAt(t *testing.T, dir string, emb *fakeProvider) *harness {
	t.Helper()
	h := &harness{t: t, dir: dir, emb: emb}
	h.start()
	return h
}

func newHarnessWithAuth(t *testing.T, auth *api.Auth) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), emb: &fakeProvider{}, auth: auth}
	h.start()
	return h
}

func (h *harness) start() {
	h.t.Helper()
	st, err := store.Open(h.dir)
	if err != nil {
		h.t.Fatalf("open store: %v", err)
	}
	h.st = st
	apiSrv := api.New(st, embed.Provider(h.emb), api.WithAuth(h.auth))
	mcpSrv := mcp.New(apiSrv, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpSrv)
	mux.Handle("/", apiSrv.Handler())
	h.srv = httptest.NewServer(api.OriginGuard(mux, nil))
	h.httpURL = h.srv.URL + "/v1"
	h.mcpURL = h.srv.URL + "/mcp"
	// Registered after the t.TempDir cleanup (LIFO order), so the server,
	// store handles, and SQLite files are closed before the directory is
	// removed — otherwise TempDir removal can fail on open files (Windows).
	// After reopen() this closes the already-closed earlier incarnation too;
	// both closes are safe to repeat.
	h.t.Cleanup(h.close)
}

// reopen simulates a server restart on the same data directory: the store is
// closed, a fresh process-equivalent server is built, and all later calls go
// through it. Used for durability assertions.
func (h *harness) reopen() {
	h.t.Helper()
	h.srv.Close()
	if err := h.st.Close(); err != nil {
		h.t.Fatalf("close store: %v", err)
	}
	h.start()
}

// close tears the server down without the test-cleanup hook (for tests that
// reopen manually).
func (h *harness) close() {
	h.srv.Close()
	_ = h.st.Close()
}

// httpCall POSTs an operation to /v1/{op} and decodes the JSON envelope.
func (h *harness) httpCall(op string, body any) (int, map[string]any) {
	h.t.Helper()
	return h.httpCallWithHeaders(op, body, nil)
}

// httpCallWithHeaders is httpCall with extra request headers.
func (h *harness) httpCallWithHeaders(op string, body any, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("marshal %s body: %v", op, err)
	}
	req, err := http.NewRequest(http.MethodPost, h.httpURL+"/"+op, bytes.NewReader(raw))
	if err != nil {
		h.t.Fatalf("new request /v1/%s: %v", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("post /v1/%s: %v", op, err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode /v1/%s response: %v", op, err)
	}
	return res.StatusCode, out
}

// httpCallRaw is httpCall for pre-encoded bodies (malformed JSON cases).
func (h *harness) httpCallRaw(op, body, contentType string) (*http.Response, string) {
	h.t.Helper()
	res, err := http.Post(h.httpURL+"/"+op, contentType, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("post /v1/%s: %v", op, err)
	}
	defer res.Body.Close()
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		h.t.Fatalf("read /v1/%s body: %v", op, err)
	}
	return res, string(buf)
}

// mcpResult is the decoded outcome of one JSON-RPC request.
type mcpResult struct {
	status int            // HTTP status of the response
	proto  map[string]any // JSON-RPC error object, nil for successful calls
	result map[string]any // tools/call result, nil on protocol errors
}

// isError reports the tools/call isError flag (false for protocol errors,
// which never reach the tool).
func (r mcpResult) isError() bool {
	if r.result == nil {
		return false
	}
	isErr, _ := r.result["isError"].(bool)
	return isErr
}

// structured returns the structuredContent payload of a successful call.
func (r mcpResult) structured() map[string]any {
	if r.result == nil {
		return nil
	}
	sc, _ := r.result["structuredContent"].(map[string]any)
	return sc
}

// toolError parses the error envelope a failing tool call reports as its text
// content. Nil when the call did not fail.
func (r mcpResult) toolError() map[string]any {
	if !r.isError() {
		return nil
	}
	content, _ := r.result["content"].([]any)
	if len(content) == 0 {
		return nil
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return nil
	}
	return env
}

var mcpCallID int

// mcpCall invokes a tool over MCP tools/call.
func (h *harness) mcpCall(op string, args any) mcpResult {
	h.t.Helper()
	return h.mcpCallWithHeaders(op, args, nil)
}

// mcpCallWithHeaders is mcpCall with extra HTTP headers on the JSON-RPC request.
func (h *harness) mcpCallWithHeaders(op string, args any, headers map[string]string) mcpResult {
	h.t.Helper()
	return h.rpcWithHeaders(map[string]any{
		"jsonrpc": "2.0",
		"id":      mcpNextID(),
		"method":  "tools/call",
		"params":  map[string]any{"name": op, "arguments": args},
	}, headers)
}

func mcpNextID() int {
	mcpCallID++
	return mcpCallID
}

// rpc sends one JSON-RPC message to /mcp.
func (h *harness) rpc(msg any) mcpResult {
	h.t.Helper()
	return h.rpcWithHeaders(msg, nil)
}

// rpcWithHeaders is rpc with extra HTTP headers.
func (h *harness) rpcWithHeaders(msg any, headers map[string]string) mcpResult {
	h.t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		h.t.Fatalf("marshal rpc: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, h.mcpURL, bytes.NewReader(raw))
	if err != nil {
		h.t.Fatalf("new request /mcp: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("post /mcp: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusAccepted {
		return mcpResult{status: res.StatusCode}
	}
	var decoded map[string]any
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		h.t.Fatalf("decode rpc response: %v", err)
	}
	out := mcpResult{status: res.StatusCode}
	if e, ok := decoded["error"].(map[string]any); ok {
		out.proto = e
		return out
	}
	out.result, _ = decoded["result"].(map[string]any)
	return out
}

// mustHTTP runs an operation over HTTP and returns its data object, failing
// the test on any transport or application error.
func (h *harness) mustHTTP(op string, body any) map[string]any {
	h.t.Helper()
	status, out := h.httpCall(op, body)
	if status != http.StatusOK || out["ok"] != true {
		h.t.Fatalf("/v1/%s failed: status %d %v", op, status, out)
	}
	data, ok := out["data"].(map[string]any)
	if !ok {
		h.t.Fatalf("/v1/%s returned no data object: %v", op, out)
	}
	return data
}

// mustMCP runs a tool over MCP and returns its structuredContent, failing the
// test on any protocol or tool error.
func (h *harness) mustMCP(op string, args any) map[string]any {
	h.t.Helper()
	res := h.mcpCall(op, args)
	if res.status != http.StatusOK || res.proto != nil || res.isError() {
		h.t.Fatalf("tools/call %s failed: %+v", op, res)
	}
	sc := res.structured()
	if sc == nil {
		h.t.Fatalf("tools/call %s returned no structuredContent: %+v", op, res)
	}
	return sc
}

// volatileRe matches the documented server-assigned timestamps: created_at on
// every row and "at" on migration history entries. They are the only values
// allowed to differ between two servers given identical inputs.
var volatileKeys = map[string]bool{"created_at": true, "at": true}

// createdAtRe is the documented created_at shape: a UTC millisecond
// RFC3339 timestamp (SQLite strftime('%Y-%m-%dT%H:%M:%fZ','now')).
var createdAtRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

// maskVolatile replaces server-assigned timestamp values with a placeholder so
// responses from two independently-created stores compare equal. Before
// masking, created_at must carry its documented shape — an unconditional
// replace would let a serialization regression compare equal on both
// transports.
func maskVolatile(t *testing.T, v any) any {
	switch row := v.(type) {
	case map[string]any:
		for k, val := range row {
			if !volatileKeys[k] {
				row[k] = maskVolatile(t, val)
				continue
			}
			if k == "created_at" {
				s, ok := val.(string)
				if !ok || !createdAtRe.MatchString(s) {
					t.Errorf("created_at %v does not match the documented UTC millisecond RFC3339 shape", val)
					row[k] = "<volatile>"
					continue
				}
			}
			row[k] = "<volatile>"
		}
		return row
	case []any:
		for i, val := range row {
			row[i] = maskVolatile(t, val)
		}
		return row
	default:
		return v
	}
}

// assertJSONEqual fails the test with a compact diff when two decoded JSON
// values differ.
func assertJSONEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	gotJSON, _ := json.MarshalIndent(got, "", "  ")
	wantJSON, _ := json.MarshalIndent(want, "", "  ")
	t.Fatalf("%s mismatch:\ngot:  %s\nwant: %s", what, gotJSON, wantJSON)
}

// float returns v as a float64, failing on a non-number.
func float(t *testing.T, what string, v any) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s: expected a number, got %T %v", what, v, v)
	}
	return f
}

// int64val returns v as an int64, requiring an integral JSON representation
// (the typed-read contract returns integers without a decimal point).
func int64val(t *testing.T, what string, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			t.Fatalf("%s: expected an integer, got %v", what, n)
		}
		return int64(n)
	default:
		t.Fatalf("%s: expected an integer, got %T %v", what, v, v)
		return 0
	}
}

// wantMessage fails unless the message matches the pinned shape.
func wantMessage(t *testing.T, what, msg, pattern string) {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("bad message pattern %q: %v", pattern, err)
	}
	if !re.MatchString(msg) {
		t.Fatalf("%s: message %q does not match pinned shape %q", what, msg, pattern)
	}
}

// postWithOrigin sends a /v1 operation request carrying an Origin header
// (CORS guard cases).
func (h *harness) postWithOrigin(origin, op, body string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.httpURL+"/"+op, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("post with origin: %v", err)
	}
	return res
}

// postMCPRaw sends a raw body to /mcp (malformed-JSON protocol cases).
func (h *harness) postMCPRaw(body string) *http.Response {
	h.t.Helper()
	res, err := http.Post(h.mcpURL, "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("post /mcp: %v", err)
	}
	return res
}

// decodeJSON decodes an already-fetched response body.
func decodeJSON(t *testing.T, res *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// outOfBand opens the namespace's SQLite file directly, the way a second
// process (backup tool, stray writer) would, and runs fn on the connection.
// Used only to plant fixtures the API itself refuses — corrupt vectors.
func (h *harness) outOfBand(ns string, fn func(db *sqlDB) error) {
	h.t.Helper()
	db, err := openSQL(h.dir + "/" + ns + ".db")
	if err != nil {
		h.t.Fatalf("open %s.db out of band: %v", ns, err)
	}
	defer db.Close()
	if err := fn(db); err != nil {
		h.t.Fatalf("out-of-band write: %v", err)
	}
}

// seedTable creates ns.table with fields over HTTP, failing the test on
// anything but success.
func (h *harness) seedTable(ns, table string, fields []map[string]any) map[string]any {
	h.t.Helper()
	return h.mustHTTP("create_table", map[string]any{
		"namespace": ns,
		"table":     table,
		"fields":    fields,
	})
}
