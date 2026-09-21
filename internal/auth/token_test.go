package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func testKeyring(t *testing.T) Keyring {
	t.Helper()
	k, err := NewSigningKey()
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	dep, err := NewDeploymentID()
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}
	return Keyring{Deployment: dep, Active: k}
}

func TestTokenRoundTrip(t *testing.T) {
	k := testKeyring(t)
	now := time.Now()
	tok, err := MintToken(k, "oidc:v1:abc:sub-1", []string{"oidc:v1:abc:team"}, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !LooksLikeToken(tok) {
		t.Fatalf("token %q does not carry the structural separators dispatch routes on", tok)
	}
	id, err := VerifyToken(k, tok, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.Principal != "oidc:v1:abc:sub-1" || id.Source != OIDCSourceName {
		t.Fatalf("identity %+v", id)
	}
	if len(id.Groups) != 1 || id.Groups[0] != "oidc:v1:abc:team" {
		t.Fatalf("groups %v", id.Groups)
	}
}

func TestTokenRejectsAnotherDeployment(t *testing.T) {
	a := testKeyring(t)
	now := time.Now()
	tok, err := MintToken(a, "oidc:v1:abc:sub-1", nil, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	clone := Keyring{Deployment: "a-different-deployment", Active: a.Active}
	if _, err := VerifyToken(clone, tok, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("a deployment that shares the keyring accepted another deployment's token")
	}
}

func TestTokenRejectsExpiryAndTampering(t *testing.T) {
	k := testKeyring(t)
	now := time.Now()
	tok, err := MintToken(k, "oidc:v1:abc:sub-1", nil, MinTokenTTL, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if _, err := VerifyToken(k, tok, now.Add(MinTokenTTL+time.Second)); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("an expired token verified")
	}

	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
	if _, err := VerifyToken(k, tampered, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("a token with a replaced signature verified")
	}

	other := testKeyring(t)
	other.Deployment = k.Deployment
	if _, err := VerifyToken(other, tok, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("a token signed by an unknown key verified")
	}
}

func TestTokenRejectsUnknownVersionRatherThanGuessing(t *testing.T) {
	k := testKeyring(t)
	now := time.Now()
	raw := `{"v":2,"iss":"` + k.Deployment + `","sub":"oidc:v1:abc:s","grp":[],"iat":1,"exp":99999999999}`
	header := `{"typ":"` + TokenType + `","alg":"` + TokenAlg + `","kid":"` + k.Active.ID + `"}`
	signing := b64([]byte(header)) + "." + b64([]byte(raw))
	forged := signing + "." + b64(signEd25519(k, signing))
	if _, err := VerifyToken(k, forged, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("a token carrying an unsupported format version verified")
	}
}

func TestTokenRotationVerifiesThePredecessor(t *testing.T) {
	old := testKeyring(t)
	now := time.Now()
	tok, err := MintToken(old, "oidc:v1:abc:sub-1", nil, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	successor, err := NewSigningKey()
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	rotated := Keyring{Deployment: old.Deployment, Active: successor, Verify: []SigningKey{old.Active}}
	if _, err := VerifyToken(rotated, tok, now); err != nil {
		t.Fatalf("a live token stopped verifying across a rotation overlap: %v", err)
	}

	retired := Keyring{Deployment: old.Deployment, Active: successor}
	if _, err := VerifyToken(retired, tok, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("a retired key still verified")
	}
}

func TestTokenRejectsTheReservedPrincipal(t *testing.T) {
	k := testKeyring(t)
	now := time.Now()
	tok, err := MintToken(k, AdminPrincipal, nil, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := VerifyToken(k, tok, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("a token bearing the bootstrap principal verified")
	}
}

func TestTokenTTLRange(t *testing.T) {
	for _, d := range []time.Duration{MinTokenTTL, DefaultTokenTTL, MaxTokenTTL} {
		if err := ValidateTokenTTL(d); err != nil {
			t.Fatalf("%s rejected: %v", d, err)
		}
	}
	for _, d := range []time.Duration{0, time.Minute, MaxTokenTTL + time.Hour} {
		if err := ValidateTokenTTL(d); err == nil {
			t.Fatalf("%s accepted", d)
		}
	}
}

func TestIssuerQualification(t *testing.T) {
	d := IssuerDigest("https://login.example.com/")
	if len(d) != OIDCDigestLen {
		t.Fatalf("digest %q is %d characters, want %d", d, len(d), OIDCDigestLen)
	}
	if d != IssuerDigest("https://login.example.com/") {
		t.Fatal("the digest is not deterministic")
	}
	if d == IssuerDigest("https://login.example.com") {
		t.Fatal("a trailing slash produced the same digest, so two issuers would collide")
	}

	got, err := QualifyOIDC(d, "00u1a2b3")
	if err != nil {
		t.Fatalf("qualify: %v", err)
	}
	if !strings.HasPrefix(got, "oidc:v1:"+d+":") || !IsOIDCQualified(got) {
		t.Fatalf("qualified form %q", got)
	}
	if digest, ok := OIDCIssuerOf(got); !ok || digest != d {
		t.Fatalf("issuer not recoverable from %q", got)
	}

	if _, err := QualifyOIDC(d, strings.Repeat("s", 300)); err == nil {
		t.Fatal("a claim too long to name in a grant was accepted")
	}
	if _, err := QualifyOIDC(d, "with space"); err == nil {
		t.Fatal("a claim with a space was accepted")
	}
	if _, err := QualifyOIDC(d, ""); err == nil {
		t.Fatal("an empty claim was accepted")
	}
	if _, err := QualifyOIDCGroup(d, "a,b"); err == nil {
		t.Fatal("a group claim containing the separator was accepted")
	}
}

func TestQualifiedIdentitiesAreDisjointAcrossIssuers(t *testing.T) {
	a, _ := QualifyOIDC(IssuerDigest("https://a.example"), "same-sub")
	b, _ := QualifyOIDC(IssuerDigest("https://b.example"), "same-sub")
	if a == b {
		t.Fatal("the same subject at two issuers produced one principal")
	}
}

func TestOIDCReachabilityRejectsAStaleIssuerQualification(t *testing.T) {
	ctx := context.Background()
	oldDigest := IssuerDigest("https://old.example")
	newDigest := IssuerDigest("https://new.example")

	a, err := New(Config{Mode: ModeOn})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	a.UseTokens(Keyring{Deployment: "dep"})
	a.SetOIDCIssuer(newDigest)

	stale, _ := QualifyOIDC(oldDigest, "boss")
	err = a.CheckRootAdministrator(ctx, fakeRootAdmins{{Type: SubjectPrincipal, ID: stale}})
	if err == nil {
		t.Fatal("a root grant qualified by a retired issuer was accepted as reachable")
	}
	if !strings.Contains(err.Error(), stale) {
		t.Fatalf("the startup error does not name the stranded grant: %v", err)
	}

	current, _ := QualifyOIDC(newDigest, "boss")
	if err := a.CheckRootAdministrator(ctx, fakeRootAdmins{{Type: SubjectPrincipal, ID: current}}); err != nil {
		t.Fatalf("a root grant under the configured issuer should be reachable: %v", err)
	}

	if err := a.CheckRootAdministrator(ctx, fakeRootAdmins{{Type: SubjectPrincipal, ID: "plain-principal"}}); err == nil {
		t.Fatal("with only the OIDC source enabled, an unqualified gateway-era principal is not producible and must not count as reachable")
	}

	withGateway, err := New(Config{Mode: ModeOn, TrustedProxies: proxies(t, "127.0.0.0/8")})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	withGateway.UseTokens(Keyring{Deployment: "dep"})
	withGateway.SetOIDCIssuer(newDigest)
	if err := withGateway.CheckRootAdministrator(ctx, fakeRootAdmins{{Type: SubjectPrincipal, ID: "plain-principal"}}); err != nil {
		t.Fatalf("a gateway can assert any well-formed principal, so it stays reachable: %v", err)
	}
}

func TestAStalledRefreshDoesNotOverwriteARotation(t *testing.T) {
	before := testKeyring(t)
	successor, err := NewSigningKey()
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	after := Keyring{Deployment: before.Deployment, Active: successor}

	started := make(chan struct{})
	release := make(chan struct{})
	ts := &tokenSource{
		ring:     before,
		loadedAt: time.Now().Add(-time.Hour),
		every:    time.Millisecond,
		now:      time.Now,
	}
	ts.load = func(context.Context) (Keyring, error) {
		close(started)
		<-release
		return before, nil
	}

	done := make(chan Keyring, 1)
	go func() { done <- ts.keyring() }()
	<-started
	ts.replace(after)
	close(release)

	if got := <-done; got.Active.ID != after.Active.ID {
		t.Fatalf("the stalled refresh handed its caller the pre-rotation ring: %q, want %q", got.Active.ID, after.Active.ID)
	}
	ts.mu.RLock()
	held, at := ts.ring, ts.loadedAt
	ts.mu.RUnlock()
	if held.Active.ID != after.Active.ID {
		t.Fatalf("a refresh that read the pre-rotation ring overwrote the rotation, so a retired key verifies again: %q, want %q", held.Active.ID, after.Active.ID)
	}
	if at.Before(time.Now().Add(-time.Second)) {
		t.Fatal("the discarded refresh pushed loadedAt backwards")
	}
}
