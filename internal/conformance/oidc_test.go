package conformance

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/auth"
)

type issuerStub struct {
	srv         *httptest.Server
	sub         string
	groups      []string
	lastPKCE    string
	lastRedir   string
	challenges  map[string]string
	extraClaims map[string]any
	plainField  string
}

func newIssuerStub(t *testing.T, sub string, groups []string) *issuerStub {
	t.Helper()
	s := &issuerStub{sub: sub, groups: groups, challenges: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"authorization_endpoint": s.srv.URL + "/authorize",
			"token_endpoint":         s.srv.URL + "/token",
			"userinfo_endpoint":      s.srv.URL + "/userinfo",
		}
		if s.plainField != "" {
			doc[s.plainField] = strings.Replace(doc[s.plainField].(string), "https://", "http://", 1)
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.lastPKCE = q.Get("code_challenge")
		s.lastRedir = q.Get("redirect_uri")
		if q.Get("code_challenge_method") != "S256" {
			http.Error(w, "PKCE S256 is required", http.StatusBadRequest)
			return
		}
		s.challenges["the-code"] = q.Get("code_challenge")
		http.Redirect(w, r, q.Get("redirect_uri")+"?state="+url.QueryEscape(q.Get("state"))+"&code=the-code", http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		verifier := r.Form.Get("code_verifier")
		if verifier == "" {
			http.Error(w, "missing code_verifier", http.StatusBadRequest)
			return
		}
		claims := map[string]any{"sub": s.sub, "iss": s.srv.URL, "aud": "dolmen-test"}
		if len(s.groups) > 0 {
			claims["groups"] = s.groups
		}
		for k, v := range s.extraClaims {
			claims[k] = v
		}
		raw, _ := json.Marshal(claims)
		idToken := "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": idToken, "access_token": "at"})
	})
	s.srv = httptest.NewTLSServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *issuerStub) client() *http.Client {
	c := s.srv.Client()
	c.Timeout = 15 * time.Second
	return c
}

func oidcHarness(t *testing.T, stub *issuerStub) *harness {
	t.Helper()
	h := newHarnessMode(t, authAdminKey)
	cfg := auth.OIDCConfig{
		Issuer:       stub.srv.URL,
		ClientID:     "dolmen-test",
		ClientSecret: "shhh",
	}
	src := auth.NewOIDCSource(cfg, h.grants, h.keyring(t), stub.client())
	h.client = stub.client()
	h.attachOIDC(t, src)
	return h
}

func TestOIDCDanceIssuesAUsableToken(t *testing.T) {
	stub := newIssuerStub(t, "00u1a2b3", []string{"platform"})
	h := oidcHarness(t, stub)

	client := h.web()
	res, err := client.Get(h.srv.URL + "/v1/auth/begin")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the dance ended with status %d", res.StatusCode)
	}
	body := readAll(t, res)
	if stub.lastPKCE == "" {
		t.Fatal("the authorization request carried no PKCE challenge")
	}

	token := extractToken(t, body)
	status, out := h.httpCallAs(identity{bearer: token}, "whoami", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("the issued token does not authenticate: status %d %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	principal, _ := data["principal"].(string)
	wantPrefix := "oidc:v1:" + auth.IssuerDigest(stub.srv.URL) + ":"
	if !strings.HasPrefix(principal, wantPrefix) || !strings.HasSuffix(principal, "00u1a2b3") {
		t.Fatalf("principal %q is not issuer-qualified as %q...", principal, wantPrefix)
	}
	if data["source"] != "oidc" {
		t.Fatalf("source %v, want oidc", data["source"])
	}
	groups, _ := data["groups"].([]any)
	if len(groups) != 1 || !strings.HasSuffix(groups[0].(string), ":platform") {
		t.Fatalf("groups %v are not issuer-qualified", groups)
	}
}

func TestOIDCTokenGrantsNothingByItself(t *testing.T) {
	stub := newIssuerStub(t, "00u1a2b3", nil)
	h := oidcHarness(t, stub)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})

	token := danceForToken(t, h)
	status, out := h.httpCallAs(identity{bearer: token}, "query", map[string]any{"namespace": "acme", "sql": "SELECT 1"})
	if status != http.StatusForbidden {
		t.Fatalf("an ungranted token: status %d, want 403: %v", status, out)
	}

	h.mustHTTP("grant", map[string]any{
		"subject": map[string]any{"type": "principal", "id": "oidc:v1:" + auth.IssuerDigest(stub.srv.URL) + ":00u1a2b3"},
		"object":  map[string]any{"namespace": "acme"},
		"verbs":   []string{"read"},
	})
	status, out = h.httpCallAs(identity{bearer: token}, "query", map[string]any{"namespace": "acme", "sql": "SELECT 1 AS n"})
	if status != http.StatusOK {
		t.Fatalf("a granted token: status %d %v", status, out)
	}
}

func TestOIDCStateIsSingleUse(t *testing.T) {
	stub := newIssuerStub(t, "00u1a2b3", nil)
	h := oidcHarness(t, stub)

	noRedirect := *h.web()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := noRedirect.Get(h.srv.URL + "/v1/auth/begin")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res.Body.Close()
	target, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatalf("redirect target: %v", err)
	}
	state := target.Query().Get("state")
	if state == "" {
		t.Fatal("the authorization request carried no state")
	}

	callback := fmt.Sprintf("%s/v1/auth/callback?state=%s&code=the-code", h.srv.URL, url.QueryEscape(state))
	first, err := h.web().Get(callback)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first callback: status %d", first.StatusCode)
	}

	second, err := h.web().Get(callback)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode == http.StatusOK {
		t.Fatal("the same state was accepted twice, so an intercepted callback could be replayed")
	}
}

func TestOIDCUnknownStateIsRefused(t *testing.T) {
	stub := newIssuerStub(t, "00u1a2b3", nil)
	h := oidcHarness(t, stub)

	res, err := h.web().Get(h.srv.URL + "/v1/auth/callback?state=never-issued&code=the-code")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatal("a callback carrying a state this server never issued was accepted")
	}
}

func TestAuthEndpointsAbsentWithoutTheSource(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	for _, path := range []string{"/v1/auth/begin", "/v1/auth/callback"} {
		res, err := h.web().Get(h.srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s without an OIDC source: status %d, want 404", path, res.StatusCode)
		}
	}
	if doc := h.mustHTTPGet(t, "/v1/openapi.json"); strings.Contains(doc, "/v1/auth/begin") {
		t.Fatal("openapi.json advertises the sign-in routes on a deployment that does not serve them")
	}
}

func TestAuthEndpointsAreDiscoverableWhenServed(t *testing.T) {
	stub := newIssuerStub(t, "00u1a2b3", nil)
	h := oidcHarness(t, stub)

	var doc struct {
		Paths map[string]struct {
			Get *struct {
				OperationID string `json:"operationId"`
				Summary     string `json:"summary"`
				Responses   map[string]struct {
					Content map[string]any `json:"content"`
				} `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(h.mustHTTPGet(t, "/v1/openapi.json")), &doc); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	for path, wantID := range map[string]string{"/v1/auth/begin": "auth_begin", "/v1/auth/callback": "auth_callback"} {
		entry, ok := doc.Paths[path]
		if !ok {
			t.Fatalf("openapi.json omits %s, so a client working from it alone cannot find the sign-in", path)
		}
		if entry.Get == nil {
			t.Fatalf("%s must be documented as a GET", path)
		}
		if entry.Get.OperationID != wantID {
			t.Fatalf("%s operationId = %q, want %q", path, entry.Get.OperationID, wantID)
		}
		if entry.Get.Summary == "" {
			t.Fatalf("%s carries no summary", path)
		}
	}
	callback := doc.Paths["/v1/auth/callback"].Get
	if _, ok := callback.Responses["200"].Content["text/html"]; !ok {
		t.Fatalf("the callback must advertise its HTML page, not a JSON envelope: %v", callback.Responses["200"])
	}
	if _, ok := doc.Paths["/v1/auth/begin"].Get.Responses["302"]; !ok {
		t.Fatal("the begin route must advertise its redirect")
	}
}

func TestDiscoveredEndpointsMustBeHTTPS(t *testing.T) {
	for _, field := range []string{"token_endpoint", "authorization_endpoint", "userinfo_endpoint"} {
		t.Run(field, func(t *testing.T) {
			stub := newIssuerStub(t, "00u1a2b3", nil)
			stub.plainField = field
			h := oidcHarness(t, stub)

			res, err := h.web().Get(h.srv.URL + "/v1/auth/begin")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode == http.StatusOK {
				t.Fatalf("a provider advertising a cleartext %s signed someone in: the client secret and the identity claims would cross the network readable and rewritable", field)
			}
			body := readAll(t, res)
			if !strings.Contains(body, "https") {
				t.Fatalf("the refusal does not name the requirement: %s", body)
			}
		})
	}
}

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	buf := make([]byte, 1<<16)
	n, _ := res.Body.Read(buf)
	return string(buf[:n])
}

func extractToken(t *testing.T, page string) string {
	t.Helper()
	const open = "<pre>"
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatalf("the sign-in page carries no token block: %s", page)
	}
	rest := page[i+len(open):]
	j := strings.Index(rest, "</pre>")
	if j < 0 {
		t.Fatalf("the token block is unterminated: %s", page)
	}
	tok := strings.TrimSpace(rest[:j])
	if !auth.LooksLikeToken(tok) {
		t.Fatalf("extracted %q, which is not a token", tok)
	}
	return tok
}

func danceForToken(t *testing.T, h *harness) string {
	t.Helper()
	res, err := h.web().Get(h.srv.URL + "/v1/auth/begin")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dance ended with status %d", res.StatusCode)
	}
	return extractToken(t, readAll(t, res))
}

func TestOIDCRejectsOverLimitGroups(t *testing.T) {
	many := make([]string, 5)
	for i := range many {
		many[i] = fmt.Sprintf("team-%d", i)
	}
	stub := newIssuerStub(t, "00u1a2b3", many)
	h := newHarnessMode(t, authAdminKey)
	cfg := auth.OIDCConfig{
		Issuer:       stub.srv.URL,
		ClientID:     "dolmen-test",
		ClientSecret: "shhh",
		MaxGroups:    2,
	}
	h.client = stub.client()
	h.attachOIDC(t, auth.NewOIDCSource(cfg, h.grants, h.keyring(t), stub.client()))

	res, err := h.web().Get(h.srv.URL + "/v1/auth/begin")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatal("a provider returning more groups than the server accepts signed in; dropping some would discard a group that carries a grant")
	}
	if body := readAll(t, res); !strings.Contains(body, "groups") {
		t.Fatalf("the refusal does not explain the cause: %s", body)
	}
}

func TestRotatingTheSigningKeyInvalidatesTokensWhenAsked(t *testing.T) {
	stub := newIssuerStub(t, "00u1a2b3", nil)
	h := oidcHarness(t, stub)
	token := danceForToken(t, h)

	if status, out := h.httpCallAs(identity{bearer: token}, "whoami", map[string]any{}); status != http.StatusOK {
		t.Fatalf("token does not authenticate before rotation: %d %v", status, out)
	}

	h.mustHTTP("rotate_signing_key", map[string]any{})
	if status, out := h.httpCallAs(identity{bearer: token}, "whoami", map[string]any{}); status != http.StatusOK {
		t.Fatalf("a plain rotation must keep live tokens working during the overlap: %d %v", status, out)
	}

	data := h.mustHTTP("rotate_signing_key", map[string]any{"retire_previous": true})
	if data["retired_previous"] != true {
		t.Fatalf("rotate reported %v", data)
	}
	if status, _ := h.httpCallAs(identity{bearer: token}, "whoami", map[string]any{}); status != http.StatusUnauthorized {
		t.Fatalf("retiring the predecessor left its tokens working: status %d", status)
	}

	fresh := danceForToken(t, h)
	if status, out := h.httpCallAs(identity{bearer: fresh}, "whoami", map[string]any{}); status != http.StatusOK {
		t.Fatalf("a token minted after rotation does not authenticate: %d %v", status, out)
	}
}

func TestRotateSigningKeyAbsentWithoutTheOIDCSource(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	status, out := h.httpCall("rotate_signing_key", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("rotate_signing_key without native sign-in: status %d, want 404: %v", status, out)
	}
	if doc := h.mustHTTPGet(t, "/v1/openapi.json"); strings.Contains(doc, "rotate_signing_key") {
		t.Fatal("openapi.json advertises rotate_signing_key with no signing key to rotate")
	}
}

func TestSignInErrorPageIsNotDoubleEscaped(t *testing.T) {
	stub := newIssuerStub(t, "00u1a2b3", nil)
	h := oidcHarness(t, stub)

	res, err := h.web().Get(h.srv.URL + "/v1/auth/callback?state=never-issued&code=the-code")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer res.Body.Close()
	body := readAll(t, res)
	if strings.Contains(body, "&amp;#") {
		t.Fatalf("the error page escapes its message twice, so remediation text is garbled: %s", body)
	}
	if !strings.Contains(body, "expired") && !strings.Contains(body, "unknown") {
		t.Fatalf("the error page does not carry the remediation: %s", body)
	}
}

func TestSignInAfterARotationElsewhereMintsWithTheLiveKey(t *testing.T) {
	stub := newIssuerStub(t, "00u1a2b3", nil)
	h := oidcHarness(t, stub)

	if _, err := h.grants.RotateSigningKey(t.Context(), h.keyring(t).Deployment, true); err != nil {
		t.Fatalf("rotate as another replica would: %v", err)
	}

	token := danceForToken(t, h)
	status, out := h.httpCallAs(identity{bearer: token}, "whoami", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("a sign-in completed after a rotation committed elsewhere handed out a dead token: status %d %v", status, out)
	}
}

func TestOIDCRejectsATokenFromAnotherIssuerOrAudience(t *testing.T) {
	for name, claims := range map[string]map[string]any{
		"another issuer":  {"sub": "00u1", "iss": "https://evil.example"},
		"another client":  {"sub": "00u1", "aud": "some-other-app"},
		"already expired": {"sub": "00u1", "exp": float64(1)},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newIssuerStub(t, "00u1", nil)
			stub.extraClaims = claims
			h := oidcHarness(t, stub)

			res, err := h.web().Get(h.srv.URL + "/v1/auth/begin")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode == http.StatusOK {
				t.Fatalf("a token carrying %s was trusted for the caller's identity", name)
			}
		})
	}
}
