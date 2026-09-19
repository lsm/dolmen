package auth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func proxies(t *testing.T, raw string) []*net.IPNet {
	t.Helper()
	p, err := ParseTrustedProxies(raw)
	if err != nil {
		t.Fatalf("parse trusted proxies %q: %v", raw, err)
	}
	return p
}

func asserted(t *testing.T, peer, principal, groups string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/list_namespaces", nil)
	r.RemoteAddr = peer
	if principal != "" {
		r.Header.Set(PrincipalHeader, principal)
	}
	if groups != "" {
		r.Header.Set(GroupsHeader, groups)
	}
	return r
}

func gatewayAuth(t *testing.T, cidrs string, maxGroups int) *Authenticator {
	t.Helper()
	a, err := New(Config{Mode: ModeOn, AdminKey: testKey, TrustedProxies: proxies(t, cidrs), MaxGroups: maxGroups})
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}
	return a
}

func TestParseTrustedProxies(t *testing.T) {
	for _, raw := range []string{"10.0.0.0/8", "127.0.0.1", "2001:db8::/32", "::1", " 10.0.0.0/8 , 192.168.1.1 "} {
		if p := proxies(t, raw); len(p) == 0 {
			t.Fatalf("%q parsed to no ranges", raw)
		}
	}
	if p, err := ParseTrustedProxies(""); err != nil || p != nil {
		t.Fatalf("empty parsed to %v, %v", p, err)
	}
	for _, raw := range []string{"not-an-ip", "10.0.0.0/64", "10.0.0.0/8,garbage"} {
		if _, err := ParseTrustedProxies(raw); err == nil {
			t.Fatalf("%q accepted", raw)
		}
	}
}

func TestHeaderSourceAuthenticatesFromTrustedPeer(t *testing.T) {
	a := gatewayAuth(t, "127.0.0.0/8", 0)
	id, err := a.Authenticate(asserted(t, "127.0.0.1:5555", "alice", "team-a,readers"))
	if err != nil {
		t.Fatalf("trusted assertion rejected: %v", err)
	}
	if id.Principal != "alice" {
		t.Fatalf("principal %q, want alice", id.Principal)
	}
	if strings.Join(id.Groups, ",") != "team-a,readers" {
		t.Fatalf("groups %v", id.Groups)
	}
	if id.Source != HeaderSourceName {
		t.Fatalf("source %q, want %q", id.Source, HeaderSourceName)
	}
}

func TestHeaderSourceIgnoresUntrustedPeer(t *testing.T) {
	a := gatewayAuth(t, "10.0.0.0/8", 0)
	if _, err := a.Authenticate(asserted(t, "127.0.0.1:5555", "alice", "")); err == nil {
		t.Fatal("assertion from an untrusted peer authenticated")
	}
}

func TestHeaderSourceIgnoresForwardedFor(t *testing.T) {
	a := gatewayAuth(t, "10.0.0.0/8", 0)
	r := asserted(t, "127.0.0.1:5555", "alice", "")
	r.Header.Set("X-Forwarded-For", "10.0.0.5")
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("X-Forwarded-For established trust")
	}
}

func TestHeaderSourceRejectsMalformedIdentity(t *testing.T) {
	a := gatewayAuth(t, "127.0.0.0/8", 0)
	for name, tc := range map[string]struct{ principal, groups string }{
		"space in principal":     {"alice smith", ""},
		"non ascii principal":    {"alicé", ""},
		"too long principal":     {strings.Repeat("a", 257), ""},
		"reserved principal":     {AdminPrincipal, ""},
		"space in group":         {"alice", "team a"},
		"non ascii group":        {"alice", "téam"},
		"too long group":         {"alice", strings.Repeat("g", 129)},
		"groups without a princ": {"", "team-a"},
	} {
		if _, err := a.Authenticate(asserted(t, "127.0.0.1:5555", tc.principal, tc.groups)); err == nil {
			t.Fatalf("%s: authenticated", name)
		}
	}
}

func TestHeaderSourceNormalizesGroups(t *testing.T) {
	a := gatewayAuth(t, "127.0.0.0/8", 0)
	id, err := a.Authenticate(asserted(t, "127.0.0.1:5555", "alice", " team-a , , readers ,team-a "))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if strings.Join(id.Groups, ",") != "team-a,readers" {
		t.Fatalf("groups %v, want trimmed, empties dropped, repeats deduplicated in order", id.Groups)
	}
}

func TestHeaderSourceRejectsOverLimitGroups(t *testing.T) {
	a := gatewayAuth(t, "127.0.0.0/8", 2)
	if _, err := a.Authenticate(asserted(t, "127.0.0.1:5555", "alice", "a,b")); err != nil {
		t.Fatalf("at the limit rejected: %v", err)
	}
	if _, err := a.Authenticate(asserted(t, "127.0.0.1:5555", "alice", "a,b,c")); err == nil {
		t.Fatal("over-limit groups authenticated instead of failing the identity")
	}
}

func TestBearerOutranksAssertedHeaders(t *testing.T) {
	a := gatewayAuth(t, "127.0.0.0/8", 0)
	r := asserted(t, "127.0.0.1:5555", "alice", "team-a")
	r.Header.Set("Authorization", "Bearer "+testKey)
	id, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("admin key alongside headers rejected: %v", err)
	}
	if id.Principal != AdminPrincipal {
		t.Fatalf("principal %q, want the bearer identity %q", id.Principal, AdminPrincipal)
	}

	r = asserted(t, "127.0.0.1:5555", "alice", "team-a")
	r.Header.Set("Authorization", "Bearer wrong-but-well-shaped-credential-0000")
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("an invalid bearer silently downgraded to the asserted header identity")
	}
}

func TestAuthOffIgnoresTrustedProxies(t *testing.T) {
	a, err := New(Config{Mode: ModeOff, TrustedProxies: proxies(t, "127.0.0.0/8")})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	id, err := a.Authenticate(asserted(t, "127.0.0.1:5555", "alice", "team-a"))
	if err != nil {
		t.Fatalf("auth off rejected: %v", err)
	}
	if !id.Empty() {
		t.Fatalf("auth off produced identity %+v", id)
	}
}

type fakeRootAdmins []Subject

func (f fakeRootAdmins) RootAdmins(context.Context) ([]Subject, error) { return f, nil }

func TestTrustedProxiesWithoutAdminKeyNeedsADurableRootGrant(t *testing.T) {
	a, err := New(Config{Mode: ModeOn, TrustedProxies: proxies(t, "127.0.0.0/8")})
	if err != nil {
		t.Fatalf("auth on with a proxy source refused at construction: %v", err)
	}
	err = a.CheckRootAdministrator(context.Background(), fakeRootAdmins(nil))
	if err == nil {
		t.Fatal("no admin key and no root grant started")
	}
	if !strings.Contains(err.Error(), "root administrator") {
		t.Fatalf("error does not name the missing administrator: %v", err)
	}

	if err := a.CheckRootAdministrator(context.Background(), fakeRootAdmins{{Type: SubjectGroup, ID: "admins"}}); err == nil {
		t.Fatal("a group root grant satisfied the check, but membership is asserted per request and cannot be established at startup")
	}
	if err := a.CheckRootAdministrator(context.Background(), fakeRootAdmins{{Type: SubjectPrincipal, ID: "boss"}}); err != nil {
		t.Fatalf("a durable principal root grant did not satisfy the check: %v", err)
	}

	withKey, err := New(Config{Mode: ModeOn, AdminKey: testKey})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := withKey.CheckRootAdministrator(context.Background(), fakeRootAdmins(nil)); err != nil {
		t.Fatalf("the bootstrap key did not satisfy the check: %v", err)
	}
}

func TestMaxGroupsRange(t *testing.T) {
	for _, n := range []int{0, 1, 128, 1024} {
		if _, err := New(Config{Mode: ModeOff, MaxGroups: n}); err != nil {
			t.Fatalf("max groups %d rejected: %v", n, err)
		}
	}
	for _, n := range []int{-1, 1025} {
		if _, err := New(Config{Mode: ModeOff, MaxGroups: n}); err == nil {
			t.Fatalf("max groups %d accepted", n)
		}
	}
}
