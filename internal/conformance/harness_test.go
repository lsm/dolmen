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
	"time"

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

// harnessMode is one point of the conformance matrix (spec §8.2): how the
// booted server is configured to treat identity. authOff is the only mode
// today — the v0.2.0 server, no auth wiring, identity ignored — so the
// struct is deliberately almost empty: later slices add mode values by
// extending it here, not by reworking the harness.
type harnessMode struct {
	// name identifies the mode in failure messages ("off", later "gateway"
	// and "native+keys").
	name string

	// The future auth server options (spec §1.2–1.4, §8.2) ride here as
	// placeholder fields, each commented with the slice that activates it:
	//
	// authOn           bool     // DOLMEN_AUTH=on — activated by 7b (gateway mode)
	// trustedProxies   []string // DOLMEN_TRUSTED_PROXIES — activated by 7b
	// adminKey         string   // DOLMEN_ADMIN_KEY — activated by 7b
	// oidcIssuer       string   // DOLMEN_AUTH_OIDC_ISSUER — activated by 10g (native+keys mode)
	// oidcClientID     string   // DOLMEN_AUTH_OIDC_CLIENT_ID — activated by 10g
	// oidcClientSecret string   // DOLMEN_AUTH_OIDC_CLIENT_SECRET — activated by 10g
}

// authOff is the matrix's first mode: today's server, which must keep
// passing the v0.2.0 contract byte for byte (spec §8.1).
var authOff = harnessMode{name: "off"}

// off reports whether the mode is auth:off — the server ignores identity
// entirely. There is no other mode yet; 7b replaces this with the real
// switch.
func (m harnessMode) off() bool { return m.name == "off" }

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
	// mode is how the booted server treats identity (spec §8.2). It is set
	// once at construction and survives reopen(): a restart keeps its mode.
	mode harnessMode
	// storeOpts are extra store.Open options every (re)start applies — how
	// realtime fixtures shrink the change-log retention for expiry tests.
	storeOpts []store.OpenOption

	httpURL string // .../v1
	mcpURL  string // .../mcp
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessMode(t, authOff)
}

// newHarnessMode boots a server at one point of the conformance matrix
// (spec §8.2). Tests that do not care about the mode — the whole existing
// suite — go through newHarness, which selects authOff, so that suite is
// itself the proof the mode boot path stays v0.2.0-faithful.
func newHarnessMode(t *testing.T, mode harnessMode) *harness {
	t.Helper()
	return newHarnessAtMode(t, t.TempDir(), &fakeProvider{}, mode)
}

// newHarnessRetention boots the auth:off server with a non-default change-log
// retention (§9.3) — the realtime expiry fixtures need a window far below the
// deployment floor without waiting on wall-clock scales.
func newHarnessRetention(t *testing.T, d time.Duration) *harness {
	t.Helper()
	h := newHarnessAtMode(t, t.TempDir(), &fakeProvider{}, authOff)
	h.storeOpts = []store.OpenOption{store.WithChangeRetention(d)}
	h.reopen()
	return h
}

func newHarnessAt(t *testing.T, dir string, emb *fakeProvider) *harness {
	t.Helper()
	return newHarnessAtMode(t, dir, emb, authOff)
}

// newHarnessAtMode is the one constructor that builds a harness; the others
// are mode and convenience shorthands over it.
func newHarnessAtMode(t *testing.T, dir string, emb *fakeProvider, mode harnessMode) *harness {
	t.Helper()
	h := &harness{t: t, dir: dir, emb: emb, mode: mode}
	h.start()
	return h
}

func (h *harness) start() {
	h.t.Helper()
	st, err := store.Open(h.dir, h.storeOpts...)
	if err != nil {
		h.t.Fatalf("open store: %v", err)
	}
	h.st = st
	// The mode's auth server options apply here as their slices activate:
	// 7b wires DOLMEN_AUTH / trusted proxies / the admin key around apiSrv,
	// 10g adds the OIDC source. authOff configures nothing — the wiring
	// below is exactly v0.2.0's, which is what §8.1's byte-for-byte rule
	// pins.
	apiSrv := api.New(st, embed.Provider(h.emb))
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
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("marshal %s body: %v", op, err)
	}
	res := h.postWithHeaders(h.httpURL+"/"+op, raw, nil)
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode /v1/%s response: %v", op, err)
	}
	return res.StatusCode, out
}

// identity is a principal a test acts as, in the forms the auth sources
// assert it (spec §1.1, §1.3): the X-Dolmen header pair a trusted proxy
// sets, and/or a bearer key (an API key or the admin key). The helpers send
// exactly what a well-behaved source would; an empty identity asserts
// nothing and is the anonymous call.
type identity struct {
	principal string
	groups    []string
	bearer    string
}

// headers returns the HTTP headers asserting id the way its sources would.
func (id identity) headers() map[string]string {
	h := map[string]string{}
	if id.principal != "" {
		h["X-Dolmen-Principal"] = id.principal
	}
	if len(id.groups) > 0 {
		h["X-Dolmen-Groups"] = strings.Join(id.groups, ",")
	}
	if id.bearer != "" {
		h["Authorization"] = "Bearer " + id.bearer
	}
	return h
}

// leakStrings returns the identity strings that must never surface in an
// auth:off response: the principal, every group, and the bearer.
func (id identity) leakStrings() []string {
	out := make([]string, 0, len(id.groups)+2)
	for _, s := range append([]string{id.principal, id.bearer}, id.groups...) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// httpCallAs is httpCall carrying an identity: the assertion headers a
// gateway or key holder would send are attached, so tests can speak as a
// principal the moment later slices give the server something to do with
// one. Under auth:off the identity is still sent — the pinned invariant is
// that the server ignores it (assertIdentityIgnored) — so a mode
// misconfiguration fails in these helpers, not in whatever test trips over
// it first.
func (h *harness) httpCallAs(id identity, op string, body any) (int, map[string]any) {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("marshal %s body: %v", op, err)
	}
	res := h.postWithHeaders(h.httpURL+"/"+op, raw, id.headers())
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode /v1/%s response: %v", op, err)
	}
	if h.mode.off() {
		h.assertIdentityIgnored(id, "/v1/"+op, raw, res.StatusCode, out)
	}
	return res.StatusCode, out
}

// assertIdentityIgnored is the auth:off half of the identity-carrying
// helpers (spec §8.3, invariant 4: send the headers, assert no principal
// anywhere). The call may fail only for the operation's own reasons — the
// auth statuses are the tell: 401 does not exist under off, and a 403 today
// is only the origin guard's, which these calls (no Origin header) cannot
// trip — and no identity string may appear in the response unless the
// request body itself carried it.
func (h *harness) assertIdentityIgnored(id identity, what string, reqBody []byte, status int, payload any) {
	h.t.Helper()
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		h.t.Fatalf("%s: auth:off server answered identity with status %d — identity must be ignored in off mode", what, status)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("%s: marshal response for identity check: %v", what, err)
	}
	res, req := string(raw), string(reqBody)
	for _, s := range id.leakStrings() {
		if strings.Contains(req, s) {
			continue // the test's own payload carried the string, not the identity
		}
		if strings.Contains(res, s) {
			h.t.Fatalf("%s: auth:off response surfaces identity %q: %s", what, s, res)
		}
	}
}

// postWithHeaders POSTs a JSON body with optional extra headers attached
// (identity-carrying calls). A transport failure fails the test.
func (h *harness) postWithHeaders(url string, body []byte, hdr map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("post %s: %v", url, err)
	}
	return res
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
	return h.rpc(map[string]any{
		"jsonrpc": "2.0",
		"id":      mcpNextID(),
		"method":  "tools/call",
		"params":  map[string]any{"name": op, "arguments": args},
	})
}

// mcpCallAs is mcpCall carrying an identity (see httpCallAs): the same
// assertion headers ride the JSON-RPC POST to /mcp, and under auth:off the
// server must ignore them over this transport too.
func (h *harness) mcpCallAs(id identity, op string, args any) mcpResult {
	h.t.Helper()
	res := h.rpcHeaders(id.headers(), map[string]any{
		"jsonrpc": "2.0",
		"id":      mcpNextID(),
		"method":  "tools/call",
		"params":  map[string]any{"name": op, "arguments": args},
	})
	if h.mode.off() {
		argsRaw, _ := json.Marshal(args)
		for _, payload := range []map[string]any{res.proto, res.result} {
			if payload != nil {
				h.assertIdentityIgnored(id, "tools/call "+op, argsRaw, res.status, payload)
			}
		}
	}
	return res
}

func mcpNextID() int {
	mcpCallID++
	return mcpCallID
}

// rpc sends one JSON-RPC message to /mcp.
func (h *harness) rpc(msg any) mcpResult {
	h.t.Helper()
	return h.rpcHeaders(nil, msg)
}

// rpcHeaders is rpc with assertion headers attached (identity-carrying MCP
// calls; see mcpCallAs).
func (h *harness) rpcHeaders(hdr map[string]string, msg any) mcpResult {
	h.t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		h.t.Fatalf("marshal rpc: %v", err)
	}
	res := h.postWithHeaders(h.mcpURL, raw, hdr)
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
// every row and "at" on migration history entries — plus the change-log's
// cursor tokens (cursor, next_cursor): fresh opaque randomness per issuance
// (§9.3). These are the only values allowed to differ between two servers
// given identical inputs.
var volatileKeys = map[string]bool{"created_at": true, "at": true, "cursor": true, "next_cursor": true}

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
			if k == "cursor" || k == "next_cursor" {
				// Masked randomness still has a documented shape: a non-empty
				// opaque token. Masking unconditionally would let a vanished
				// cursor compare equal on both transports.
				s, ok := val.(string)
				if !ok || s == "" {
					t.Errorf("%s %v does not match the documented opaque non-empty cursor shape", k, val)
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

// ensureNS creates the namespace when absent, treating already-exists as
// success: the store stopped creating namespaces on first use (slice 2b's
// §6.2 rule — engines never create implicitly), so fixtures that relied on
// create-on-open create explicitly here.
func (h *harness) ensureNS(ns string) {
	h.t.Helper()
	if status, out := h.httpCall("create_namespace", map[string]any{"namespace": ns}); status != http.StatusOK {
		errEnv, _ := out["error"].(map[string]any)
		msg, _ := errEnv["message"].(string)
		if !strings.Contains(msg, "already exists") {
			h.t.Fatalf("create namespace %s: status %d %v", ns, status, out)
		}
	}
}

// seedTable creates ns.table with fields over HTTP, failing the test on
// anything but success.
func (h *harness) seedTable(ns, table string, fields []map[string]any) map[string]any {
	h.t.Helper()
	h.ensureNS(ns)
	return h.mustHTTP("create_table", map[string]any{
		"namespace": ns,
		"table":     table,
		"fields":    fields,
	})
}
