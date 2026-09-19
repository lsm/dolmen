package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
)

const testKey = "SsEXwCk7ULqcTjPy1o0hR3wcbGDHNf5IiwfT2JjdPZk"

func req(t *testing.T, bearer string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/list_namespaces", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
		bad  bool
	}{
		{in: "", want: ModeOff},
		{in: "off", want: ModeOff},
		{in: "on", want: ModeOn},
		{in: "ON", bad: true},
		{in: "true", bad: true},
	} {
		got, err := ParseMode(tc.in)
		if tc.bad {
			if err == nil {
				t.Fatalf("ParseMode(%q) succeeded, want error", tc.in)
			}
			if !strings.Contains(err.Error(), "off") || !strings.Contains(err.Error(), "on") {
				t.Fatalf("ParseMode(%q) error does not name the accepted values: %v", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("ParseMode(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestValidateAdminKeyRejectsUnusableShapes(t *testing.T) {
	if err := ValidateAdminKey(testKey); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	for name, key := range map[string]string{
		"too short":      "abc",
		"space":          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa b",
		"padding":        strings.Repeat("a", 31) + "=",
		"plus":           strings.Repeat("a", 31) + "+",
		"non ascii":      strings.Repeat("a", 31) + "é",
		"key prefix":     KeyPrefix + strings.Repeat("a", 40),
		"too long":       strings.Repeat("a", 257),
		"empty":          "",
		"newline inside": strings.Repeat("a", 31) + "\n",
	} {
		if err := ValidateAdminKey(key); err == nil {
			t.Fatalf("%s: key %q accepted, want startup error", name, key)
		}
	}
}

func TestValidateAdminKeyPrefixErrorExplainsDispatch(t *testing.T) {
	err := ValidateAdminKey(KeyPrefix + strings.Repeat("a", 40))
	if err == nil {
		t.Fatal("dlm_-prefixed key accepted")
	}
	if !strings.Contains(err.Error(), KeyPrefix) {
		t.Fatalf("error does not name the reserved prefix: %v", err)
	}
}

func TestNewRejectsAuthOnWithoutSource(t *testing.T) {
	_, err := New(Config{Mode: ModeOn})
	if err == nil {
		t.Fatal("auth on with no source started, want startup error")
	}
	if !strings.Contains(err.Error(), "DOLMEN_ADMIN_KEY") {
		t.Fatalf("error does not name the remediation: %v", err)
	}
}

func TestNewRejectsAuthOnOverStdio(t *testing.T) {
	_, err := New(Config{Mode: ModeOn, AdminKey: testKey, Stdio: true})
	if err == nil {
		t.Fatal("auth on over stdio started, want startup error")
	}
	if !strings.Contains(err.Error(), "stdio") {
		t.Fatalf("error does not name the transport: %v", err)
	}
}

func TestNewRejectsBadAdminKeyEvenWhenAuthOff(t *testing.T) {
	if _, err := New(Config{Mode: ModeOff, AdminKey: "short"}); err == nil {
		t.Fatal("malformed admin key accepted under auth off")
	}
}

func TestAuthOffAcceptsEveryRequestAndYieldsNoIdentity(t *testing.T) {
	a, err := New(Config{Mode: ModeOff})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if a.On() {
		t.Fatal("auth off reports on")
	}
	for _, bearer := range []string{"", testKey, "garbage", KeyPrefix + "whatever"} {
		id, err := a.Authenticate(req(t, bearer))
		if err != nil {
			t.Fatalf("auth off rejected bearer %q: %v", bearer, err)
		}
		if !id.Empty() {
			t.Fatalf("auth off produced identity %+v for bearer %q", id, bearer)
		}
	}
}

func TestAuthOnAdminKey(t *testing.T) {
	a, err := New(Config{Mode: ModeOn, AdminKey: testKey})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	id, err := a.Authenticate(req(t, testKey))
	if err != nil {
		t.Fatalf("admin key rejected: %v", err)
	}
	if id.Principal != AdminPrincipal {
		t.Fatalf("principal %q, want %q", id.Principal, AdminPrincipal)
	}
	if len(id.Groups) != 0 {
		t.Fatalf("admin identity carries groups %v, want none", id.Groups)
	}
}

func TestAuthOnRejectionsAreUniformAndUnauthorized(t *testing.T) {
	a, err := New(Config{Mode: ModeOn, AdminKey: testKey})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var messages []string
	for _, bearer := range []string{
		"",
		"wrong-but-well-shaped-credential-000000000",
		KeyPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"a.b.c",
		testKey + "x",
		strings.ToUpper(testKey),
	} {
		id, err := a.Authenticate(req(t, bearer))
		if err == nil {
			t.Fatalf("bearer %q authenticated as %+v", bearer, id)
		}
		if !errors.Is(err, derr.ErrUnauthorized) {
			t.Fatalf("bearer %q: error code is not unauthorized: %v", bearer, err)
		}
		messages = append(messages, err.Error())
	}
	for i, m := range messages {
		if m != messages[0] {
			t.Fatalf("rejection %d differs from the first, so the message distinguishes failures:\n%q\n%q", i, m, messages[0])
		}
		if strings.Contains(m, testKey) {
			t.Fatalf("rejection %d echoes the admin key", i)
		}
	}
}

func TestAuthOnIgnoresIdentityHeaders(t *testing.T) {
	a, err := New(Config{Mode: ModeOn, AdminKey: testKey})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := req(t, "")
	r.Header.Set("X-Dolmen-Principal", "alice")
	r.Header.Set("X-Dolmen-Groups", "admins")
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("asserted identity headers authenticated, but the header source is not built")
	}
	r = req(t, testKey)
	r.Header.Set("X-Dolmen-Principal", "alice")
	id, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("admin key with headers rejected: %v", err)
	}
	if id.Principal != AdminPrincipal {
		t.Fatalf("asserted header outranked the bearer credential: %+v", id)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	r := req(t, "")
	r.Header.Set("Authorization", "bearer "+testKey)
	if tok, ok := BearerToken(r); !ok || tok != testKey {
		t.Fatalf("lowercase scheme not accepted: %q %v", tok, ok)
	}
	r.Header.Set("Authorization", "Basic "+testKey)
	if _, ok := BearerToken(r); ok {
		t.Fatal("Basic scheme parsed as bearer")
	}
	r.Header.Set("Authorization", "Bearer")
	if _, ok := BearerToken(r); ok {
		t.Fatal("bare scheme parsed as bearer")
	}
}
