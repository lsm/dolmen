package api

import (
	"net"
	"net/http"
	"net/http/httptest"
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
