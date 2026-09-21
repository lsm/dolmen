package api

import (
	"errors"
	"html/template"
	"net"
	"net/http"
	"time"

	"github.com/lsm/dolmen/internal/auth"
)

var tokenPage = template.Must(template.New("token").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Signed in to dolmen</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;margin:0;padding:3rem 1.5rem;max-width:44rem}
code,pre{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
pre{background:#f4f4f5;padding:1rem;border-radius:.5rem;overflow-x:auto;word-break:break-all;white-space:pre-wrap}
.muted{color:#52525b}
</style></head><body>
<h1>Signed in</h1>
<p>Your token is below. It expires in {{.TTL}}. Present it on every request:</p>
<pre>{{.Token}}</pre>
<p class="muted">Example:</p>
<pre>curl -H "Authorization: Bearer {{.Token}}" {{.BaseURL}}/v1/whoami -d '{}' -H 'Content-Type: application/json'</pre>
<p class="muted">dolmen stores no session. When the token expires, sign in again.</p>
</body></html>
`))

var errorPage = template.Must(template.New("autherr").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Sign-in failed</title>
<style>body{font:16px/1.5 system-ui,sans-serif;margin:0;padding:3rem 1.5rem;max-width:44rem}</style>
</head><body><h1>Sign-in failed</h1><p>{{.Message}}</p></body></html>
`))

func (s *Server) handleAuthBegin(w http.ResponseWriter, r *http.Request) {
	src, ok := s.oidc()
	if !ok {
		writeError(w, r, notFound("unknown operation"))
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, r, &Error{Status: http.StatusMethodNotAllowed, Code: ErrCodeInvalid, Message: "use GET"})
		return
	}
	if !s.UsableHost(r) {
		writeError(w, r, errUnusableHost)
		return
	}
	target, err := src.Begin(r.Context(), s.callbackURL(r), peerOf(r))
	if err != nil {
		s.renderAuthError(w, r, err)
		return
	}
	setPublicURLCacheHeaders(w)
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	src, ok := s.oidc()
	if !ok {
		writeError(w, r, notFound("unknown operation"))
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, r, &Error{Status: http.StatusMethodNotAllowed, Code: ErrCodeInvalid, Message: "use GET"})
		return
	}
	q := r.URL.Query()
	if desc := q.Get("error"); desc != "" {
		s.renderAuthError(w, r, errors.New("the identity provider refused the sign-in"))
		return
	}
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" {
		s.renderAuthError(w, r, errors.New("the identity provider did not return a state and code"))
		return
	}
	token, ttl, err := src.Complete(r.Context(), state, code)
	if err != nil {
		s.renderAuthError(w, r, err)
		return
	}

	setPublicURLCacheHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = tokenPage.Execute(w, struct {
		Token   string
		TTL     string
		BaseURL string
	}{Token: token, TTL: humanTTL(ttl), BaseURL: s.publicContext(r).BaseURL})
}

func humanTTL(d time.Duration) string {
	days := int(d.Hours()) / 24
	if days >= 1 {
		if days == 1 {
			return "1 day"
		}
		return fmtInt(days) + " days"
	}
	return d.String()
}

func fmtInt(n int) string {
	return itoa(n)
}

func (s *Server) renderAuthError(w http.ResponseWriter, r *http.Request, err error) {
	message := "The sign-in could not be completed. Start again, and if it keeps failing ask the server's administrator to check its identity provider settings."
	if errors.Is(err, auth.ErrAuthFlow) {
		message = err.Error()
	}
	setPublicURLCacheHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = errorPage.Execute(w, struct{ Message string }{Message: message})
}

func peerOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) callbackURL(r *http.Request) string {
	return s.publicContext(r).BaseURL + auth.AuthCallbackPath
}

func (s *Server) oidc() (*auth.OIDCSource, bool) {
	if s.authn == nil || !s.authn.On() || s.oidcSource == nil {
		return nil, false
	}
	return s.oidcSource, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
