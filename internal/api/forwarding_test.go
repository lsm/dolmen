package api

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lsm/dolmen/skill"
)

func TestForwardingHeadersCountOnlyFromTrustedProxies(t *testing.T) {
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	var seen string
	h := ForwardingGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = skill.BaseURLFor(r, "")
	}), []*net.IPNet{loopback})
	send := func(peer string) string {
		r := httptest.NewRequest("GET", "http://dolmen.internal:8790/skills", nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Forwarded-Host", "evil.example")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Prefix", "/phish")
		r.Header.Set("Forwarded", "host=evil.example;proto=https")
		h.ServeHTTP(httptest.NewRecorder(), r)
		return seen
	}
	if got := send("203.0.113.9:4000"); got != "http://dolmen.internal:8790" {
		t.Fatalf("an untrusted peer steered the public URL to %s", got)
	}
	if got := send("127.0.0.1:4000"); got != "https://evil.example/phish" {
		t.Fatalf("a trusted proxy's forwarding headers must still be honored, got %s", got)
	}
}

func TestNoTrustedProxiesMeansNoForwardingHeaders(t *testing.T) {
	var seen string
	h := ForwardingGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = skill.BaseURLFor(r, "")
	}), nil)
	r := httptest.NewRequest("GET", "http://127.0.0.1:8790/skills", nil)
	r.Header.Set("X-Forwarded-Host", "evil.example")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "http://127.0.0.1:8790" {
		t.Fatalf("with no -trusted-proxies, no peer may set the public URL; got %s", seen)
	}
}

func TestDroppedForwardingHeadersAreLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	h := ForwardingGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), []*net.IPNet{loopback})
	send := func(peer string, prefix string) {
		r := httptest.NewRequest("GET", "http://dolmen.example/skills", nil)
		r.RemoteAddr = peer
		if prefix != "" {
			r.Header.Set("X-Forwarded-Prefix", prefix)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	send("10.244.1.7:4000", "")
	send("127.0.0.1:4000", "/project-abc")
	if buf.Len() != 0 {
		t.Fatalf("nothing was dropped, yet the guard logged: %s", buf.String())
	}
	send("10.244.1.7:4000", "/project-abc")
	send("10.244.1.9:4000", "/project-abc")
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 {
		t.Fatalf("want exactly one warning for dropped forwarding headers, got:\n%s", out)
	}
	for _, want := range []string{"peer=10.244.1.7", "X-Forwarded-Prefix", "DOLMEN_TRUSTED_PROXIES"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning must mention %q, got: %s", want, out)
		}
	}
}

func TestUnusablePrefixHintIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	s := New(nil, nil)
	send := func(original string) {
		r := httptest.NewRequest("GET", "https://dolmen.example/skills", nil)
		r.Header.Set("X-Forwarded-Proto", "https")
		if original != "" {
			r.Header.Set("X-Original-URI", original)
		}
		s.publicContext(r)
	}
	send("")
	send("/project-abc/skills")
	if buf.Len() != 0 {
		t.Fatalf("every prefix hint was usable, yet the server logged: %s", buf.String())
	}
	send("/project-abc/catalog")
	send("/project-abc/catalog")
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 {
		t.Fatalf("want exactly one warning for an unusable prefix hint, got:\n%s", out)
	}
	if !strings.Contains(out, "base_url=https://dolmen.example") || !strings.Contains(out, "DOLMEN_BASE_URL") {
		t.Errorf("warning must name the advertised base URL and the fix, got: %s", out)
	}
	buf.Reset()
	configured := New(nil, nil, WithBaseURL("https://dolmen.example/project-abc"))
	r := httptest.NewRequest("GET", "/skills", nil)
	r.Header.Set("X-Original-URI", "/project-abc/catalog")
	configured.publicContext(r)
	if buf.Len() != 0 {
		t.Fatalf("a configured base URL makes the hint irrelevant, yet the server logged: %s", buf.String())
	}
}
