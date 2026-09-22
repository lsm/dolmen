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
	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/mcp"
	"github.com/lsm/dolmen/internal/store"
)

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

		if t == "" {
			v[0] = 1
		}
		out[i] = v
	}
	return out, nil
}

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

type harnessMode struct {
	name           string
	adminKey       string
	trustedProxies string
	maxGroups      int
	noAdminKey     bool
}

var authOff = harnessMode{name: "off"}

var authAdminKey = harnessMode{name: "admin-key", adminKey: "Tt5vQ2rXm9LbHc0wPqZaJ4yNfE7sUgKdRi1oCnBxV3M"}

var authGateway = harnessMode{name: "gateway", adminKey: authAdminKey.adminKey, trustedProxies: "127.0.0.0/8,::1/128"}

var authGatewayNoKey = harnessMode{name: "gateway-no-key", trustedProxies: "127.0.0.0/8,::1/128", noAdminKey: true}

var authKeysOnly = harnessMode{name: "keys-only", noAdminKey: true}

func (m harnessMode) off() bool { return m.adminKey == "" && !m.noAdminKey }

func (m harnessMode) on() bool { return !m.off() }

type harness struct {
	t   *testing.T
	dir string
	srv *httptest.Server
	st  store.Engine
	emb *fakeProvider

	api    *api.Server
	grants *auth.Registry
	authn  *auth.Authenticator
	oidc   *auth.OIDCSource
	client *http.Client

	mode harnessMode

	retention *time.Duration
	apiOpts   []api.Option

	httpURL string
	mcpURL  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessMode(t, authOff)
}

func newHarnessMode(t *testing.T, mode harnessMode) *harness {
	t.Helper()
	return newHarnessAtMode(t, t.TempDir(), &fakeProvider{}, mode)
}

func newHarnessRetention(t *testing.T, d time.Duration) *harness {
	t.Helper()
	h := newHarnessAtMode(t, t.TempDir(), &fakeProvider{}, authOff)
	h.retention = &d
	h.reopen()
	return h
}

func newHarnessAge(t *testing.T, d time.Duration) *harness {
	t.Helper()
	h := newHarnessAtMode(t, t.TempDir(), &fakeProvider{}, authOff)
	h.apiOpts = []api.Option{api.WithMaxSubscriptionAge(d)}
	h.reopen()
	return h
}

func newHarnessKeepalive(t *testing.T, d time.Duration) *harness {
	t.Helper()
	h := newHarnessAtMode(t, t.TempDir(), &fakeProvider{}, authOff)
	h.apiOpts = []api.Option{api.WithKeepaliveInterval(d)}
	h.reopen()
	return h
}

func newHarnessAt(t *testing.T, dir string, emb *fakeProvider) *harness {
	t.Helper()
	return newHarnessAtMode(t, dir, emb, authOff)
}

func newHarnessAtMode(t *testing.T, dir string, emb *fakeProvider, mode harnessMode) *harness {
	t.Helper()
	h := &harness{t: t, dir: dir, emb: emb, mode: mode}
	h.start()
	return h
}

func (h *harness) start() {
	h.t.Helper()
	h.st = openEngineStoreShared(h.t, h.dir, h.retention, h.mode.authMode() != auth.ModeOff)

	trusted, err := auth.ParseTrustedProxies(h.mode.trustedProxies)
	if err != nil {
		h.t.Fatalf("parse trusted proxies for mode %q: %v", h.mode.name, err)
	}
	authn, err := auth.New(auth.Config{
		Mode:           h.mode.authMode(),
		AdminKey:       h.mode.adminKey,
		TrustedProxies: trusted,
		MaxGroups:      h.mode.maxGroups,
	})
	if err != nil {
		h.t.Fatalf("build authenticator for mode %q: %v", h.mode.name, err)
	}
	if h.authn != nil {
		authn = h.authn
	}
	opts := append(append([]api.Option(nil), h.apiOpts...), api.WithAuth(authn))
	if authn.On() {
		grants, err := auth.OpenRegistry(h.dir)
		if err != nil {
			h.t.Fatalf("open grant registry: %v", err)
		}
		h.grants = grants
		authn.UseKeys(grants)
		h.authn = authn
		opts = append(opts, api.WithGrants(grants))
	}
	apiSrv := api.New(h.st, embed.Provider(h.emb), opts...)
	h.api = apiSrv
	mcpSrv := mcp.New(apiSrv, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpSrv)
	mux.Handle("/", apiSrv.Handler())
	h.srv = httptest.NewServer(api.OriginGuard(mux, nil))
	h.httpURL = h.srv.URL + "/v1"
	h.mcpURL = h.srv.URL + "/mcp"

	h.t.Cleanup(h.close)
}

func (h *harness) reopen() {
	h.t.Helper()
	h.srv.Close()
	if err := h.st.Close(); err != nil {
		h.t.Fatalf("close store: %v", err)
	}
	if h.grants != nil {
		_ = h.grants.Close()
		h.grants = nil
	}
	h.start()
}

func (h *harness) close() {
	h.srv.Close()
	_ = h.st.Close()
	if h.grants != nil {
		_ = h.grants.Close()
		h.grants = nil
	}
}

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

type identity struct {
	principal string
	groups    []string
	bearer    string
}

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

func (id identity) leakStrings() []string {
	out := make([]string, 0, len(id.groups)+2)
	for _, s := range append([]string{id.principal, id.bearer}, id.groups...) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

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
			continue
		}
		if strings.Contains(res, s) {
			h.t.Fatalf("%s: auth:off response surfaces identity %q: %s", what, s, res)
		}
	}
}

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
	if h.mode.on() && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+h.mode.adminKey)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("post %s: %v", url, err)
	}
	return res
}

func (h *harness) httpCallRaw(op, body, contentType string) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.httpURL+"/"+op, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request /v1/%s: %v", op, err)
	}
	req.Header.Set("Content-Type", contentType)
	if h.mode.on() {
		req.Header.Set("Authorization", "Bearer "+h.mode.adminKey)
	}
	res, err := http.DefaultClient.Do(req)
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

type mcpResult struct {
	status int
	proto  map[string]any
	result map[string]any
}

func (r mcpResult) isError() bool {
	if r.result == nil {
		return false
	}
	isErr, _ := r.result["isError"].(bool)
	return isErr
}

func (r mcpResult) structured() map[string]any {
	if r.result == nil {
		return nil
	}
	sc, _ := r.result["structuredContent"].(map[string]any)
	return sc
}

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

func (h *harness) mcpCall(op string, args any) mcpResult {
	h.t.Helper()
	return h.rpc(map[string]any{
		"jsonrpc": "2.0",
		"id":      mcpNextID(),
		"method":  "tools/call",
		"params":  map[string]any{"name": op, "arguments": args},
	})
}

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

func (h *harness) rpc(msg any) mcpResult {
	h.t.Helper()
	return h.rpcHeaders(nil, msg)
}

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

var volatileKeys = map[string]bool{"created_at": true, "at": true, "cursor": true, "next_cursor": true, "expected_incarnation": true}

var createdAtRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

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
			if k == "expected_incarnation" {
				s, ok := val.(string)
				if !ok || s == "" {
					t.Errorf("expected_incarnation %v is not the documented opaque non-empty token", val)
				}
			}
			if k == "cursor" || k == "next_cursor" {

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

func assertJSONEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	gotJSON, _ := json.MarshalIndent(got, "", "  ")
	wantJSON, _ := json.MarshalIndent(want, "", "  ")
	t.Fatalf("%s mismatch:\ngot:  %s\nwant: %s", what, gotJSON, wantJSON)
}

func float(t *testing.T, what string, v any) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s: expected a number, got %T %v", what, v, v)
	}
	return f
}

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

func (h *harness) postMCPRaw(body string) *http.Response {
	h.t.Helper()
	res, err := http.Post(h.mcpURL, "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("post /mcp: %v", err)
	}
	return res
}

func decodeJSON(t *testing.T, res *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func (h *harness) outOfBand(ns string, fn func(db *sqlDB) error) {
	h.t.Helper()
	if engine := testEngine(h.t); engine != store.EngineSQLite {
		h.t.Fatalf("out-of-band surgery opens the SQLite namespace file; engine %q must skip this fixture instead", engine)
	}
	db, err := openSQL(h.dir + "/" + ns + ".db")
	if err != nil {
		h.t.Fatalf("open %s.db out of band: %v", ns, err)
	}
	defer db.Close()
	if err := fn(db); err != nil {
		h.t.Fatalf("out-of-band write: %v", err)
	}
}

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

func (h *harness) seedTable(ns, table string, fields []map[string]any) map[string]any {
	h.t.Helper()
	h.ensureNS(ns)
	return h.mustHTTP("create_table", map[string]any{
		"namespace": ns,
		"table":     table,
		"fields":    fields,
	})
}

func (m harnessMode) authMode() auth.Mode {
	if m.on() {
		return auth.ModeOn
	}
	return auth.ModeOff
}

func aliceHeaders() map[string]string {
	return map[string]string{"X-Dolmen-Principal": "alice", "X-Dolmen-Groups": "team-a,readers"}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func (h *harness) mustHTTPAs(t *testing.T, id identity, op string, body any) map[string]any {
	t.Helper()
	status, out := h.httpCallAs(id, op, body)
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("/v1/%s failed: status %d %v", op, status, out)
	}
	data, _ := out["data"].(map[string]any)
	return data
}

func (h *harness) keyring(t *testing.T) auth.Keyring {
	t.Helper()
	dep, err := h.grants.DeploymentID(t.Context(), "")
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}
	ring, err := h.grants.LoadKeyring(t.Context(), dep)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return ring
}

func (h *harness) web() *http.Client {
	if h.client != nil {
		return h.client
	}
	return http.DefaultClient
}

func (h *harness) attachOIDC(t *testing.T, src *auth.OIDCSource) {
	t.Helper()
	h.authn.UseTokens(h.keyring(t))
	src.PublishRingTo(h.authn.UseTokens)
	dep := h.keyring(t).Deployment
	h.authn.RefreshTokensFrom(func(ctx context.Context) (auth.Keyring, error) {
		return h.grants.LoadKeyring(ctx, dep)
	}, time.Millisecond)
	h.apiOpts = append(h.apiOpts, api.WithOIDC(src))
	h.oidc = src
	h.restart()
}

func (h *harness) restart() {
	h.t.Helper()
	h.srv.Close()
	apiSrv := api.New(h.st, embed.Provider(h.emb), append(append([]api.Option(nil), h.apiOpts...),
		api.WithAuth(h.authn), api.WithGrants(h.grants))...)
	h.api = apiSrv
	mcpSrv := mcp.New(apiSrv, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpSrv)
	mux.Handle("/", apiSrv.Handler())
	h.srv = httptest.NewServer(api.OriginGuard(mux, nil))
	h.httpURL = h.srv.URL + "/v1"
	h.mcpURL = h.srv.URL + "/mcp"
}
