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

	"github.com/lsm/dolmen/internal/auth"
)

type issuerStub struct {
	srv        *httptest.Server
	sub        string
	groups     []string
	lastPKCE   string
	lastRedir  string
	challenges map[string]string
}

func newIssuerStub(t *testing.T, sub string, groups []string) *issuerStub {
	t.Helper()
	s := &issuerStub{sub: sub, groups: groups, challenges: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": s.srv.URL + "/authorize",
			"token_endpoint":         s.srv.URL + "/token",
			"userinfo_endpoint":      s.srv.URL + "/userinfo",
		})
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
		claims := map[string]any{"sub": s.sub}
		if len(s.groups) > 0 {
			claims["groups"] = s.groups
		}
		raw, _ := json.Marshal(claims)
		idToken := "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": idToken, "access_token": "at"})
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func oidcHarness(t *testing.T, stub *issuerStub) *harness {
	t.Helper()
	h := newHarnessMode(t, authAdminKey)
	cfg := auth.OIDCConfig{
		Issuer:       stub.srv.URL,
		ClientID:     "dolmen-test",
		ClientSecret: "shhh",
	}
	src := auth.NewOIDCSource(cfg, h.grants, h.keyring(t), nil)
	h.attachOIDC(t, src)
	return h
}

func TestOIDCDanceIssuesAUsableToken(t *testing.T) {
	stub := newIssuerStub(t, "00u1a2b3", []string{"platform"})
	h := oidcHarness(t, stub)

	client := &http.Client{}
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

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	first, err := http.Get(callback)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first callback: status %d", first.StatusCode)
	}

	second, err := http.Get(callback)
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

	res, err := http.Get(h.srv.URL + "/v1/auth/callback?state=never-issued&code=the-code")
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
		res, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s without an OIDC source: status %d, want 404", path, res.StatusCode)
		}
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
	res, err := http.Get(h.srv.URL + "/v1/auth/begin")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dance ended with status %d", res.StatusCode)
	}
	return extractToken(t, readAll(t, res))
}
