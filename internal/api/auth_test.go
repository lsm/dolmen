package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewAuthDefaultsToOff(t *testing.T) {
	a, err := NewAuth("", "", "", "", "")
	if err != nil {
		t.Fatalf("empty mode: %v", err)
	}
	if a.On {
		t.Fatalf("empty mode should be off")
	}
}

func TestNewAuthOnRequiresIdentitySource(t *testing.T) {
	if _, err := NewAuth("on", "", "", "", ""); err == nil {
		t.Fatalf("auth on with no source must fail")
	}
}

func TestNewAuthOnWithTrustedProxy(t *testing.T) {
	a, err := NewAuth("on", "127.0.0.1/32", "", "", "")
	if err != nil {
		t.Fatalf("auth on with trusted proxy: %v", err)
	}
	if !a.On {
		t.Fatalf("auth should be on")
	}
	if a.PrincipalHeader != DefaultPrincipalHeader {
		t.Fatalf("principal header default: got %q", a.PrincipalHeader)
	}
}

func TestNewAuthAdminKeyValidation(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef"
	if _, err := NewAuth("on", "", "", "", valid); err != nil {
		t.Fatalf("valid admin key: %v", err)
	}
	for _, bad := range []string{"short", "has+padding=", "has spaces", "has.dot", "has/slash"} {
		if _, err := NewAuth("on", "", "", "", bad); err == nil {
			t.Fatalf("admin key %q should be rejected", bad)
		}
	}
}

func TestAuthAuthenticateOffIgnoresCredentials(t *testing.T) {
	a, _ := NewAuth("off", "", "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/list_tables", nil)
	req.Header.Set(DefaultPrincipalHeader, "alice")
	ctx, apiErr := a.Authenticate(req.Context(), req)
	if apiErr != nil {
		t.Fatalf("auth off should not error: %v", apiErr)
	}
	if CallerFrom(ctx) != nil {
		t.Fatalf("auth off should not attach a caller")
	}
}

func TestAuthAuthenticateOnRequiresTrustedProxy(t *testing.T) {
	a, _ := NewAuth("on", "10.0.0.0/8", "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/list_tables", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set(DefaultPrincipalHeader, "alice")
	_, apiErr := a.Authenticate(req.Context(), req)
	if apiErr == nil {
		t.Fatalf("untrusted proxy should fail")
	}
	if apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", apiErr.Status)
	}
	if apiErr.Code != ErrCodeUnauthorized {
		t.Fatalf("expected code %q, got %q", ErrCodeUnauthorized, apiErr.Code)
	}
}

func TestAuthAuthenticateOnRequiresPrincipal(t *testing.T) {
	a, _ := NewAuth("on", "127.0.0.1/32", "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/list_tables", nil)
	_, apiErr := a.Authenticate(req.Context(), req)
	if apiErr == nil {
		t.Fatalf("missing principal should fail")
	}
}

func TestAuthAuthenticateOnTrustedProxy(t *testing.T) {
	a, _ := NewAuth("on", "127.0.0.1/32", "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/list_tables", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set(DefaultPrincipalHeader, "alice")
	req.Header.Set(DefaultGroupsHeader, "admins, users")
	ctx, apiErr := a.Authenticate(req.Context(), req)
	if apiErr != nil {
		t.Fatalf("trusted proxy with valid headers: %v", apiErr)
	}
	c := CallerFrom(ctx)
	if c == nil {
		t.Fatalf("no caller attached")
	}
	if c.Principal != "alice" {
		t.Fatalf("principal: got %q, want alice", c.Principal)
	}
	if len(c.Groups) != 2 || c.Groups[0] != "admins" || c.Groups[1] != "users" {
		t.Fatalf("groups: got %v", c.Groups)
	}
}

func TestAuthAuthenticateRejectsReservedPrincipal(t *testing.T) {
	a, _ := NewAuth("on", "127.0.0.1/32", "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/list_tables", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set(DefaultPrincipalHeader, BuiltinAdminPrincipal)
	_, apiErr := a.Authenticate(req.Context(), req)
	if apiErr == nil {
		t.Fatalf("reserved principal should fail")
	}
}

func TestAuthAuthenticateAdminKey(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef"
	a, _ := NewAuth("on", "", "", "", key)
	req := httptest.NewRequest(http.MethodPost, "/v1/list_tables", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	ctx, apiErr := a.Authenticate(req.Context(), req)
	if apiErr != nil {
		t.Fatalf("valid admin key: %v", apiErr)
	}
	c := CallerFrom(ctx)
	if c == nil || c.Principal != BuiltinAdminPrincipal {
		t.Fatalf("admin principal: got %+v", c)
	}
}

func TestAuthAuthenticateAdminKeyOverridesProxyHeaders(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef"
	a, _ := NewAuth("on", "127.0.0.1/32", "", "", key)
	req := httptest.NewRequest(http.MethodPost, "/v1/list_tables", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(DefaultPrincipalHeader, "alice")
	ctx, apiErr := a.Authenticate(req.Context(), req)
	if apiErr != nil {
		t.Fatalf("admin key should win: %v", apiErr)
	}
	c := CallerFrom(ctx)
	if c.Principal != BuiltinAdminPrincipal {
		t.Fatalf("principal: got %q, want %q", c.Principal, BuiltinAdminPrincipal)
	}
}

func TestAuthAuthenticateGroupValidation(t *testing.T) {
	a, _ := NewAuth("on", "127.0.0.1/32", "", "", "")
	for _, tc := range []struct {
		name   string
		groups string
		wantOK bool
	}{
		{"simple", "a,b", true},
		{"too many", "a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q,r,s,t,u,v,w,x,y,z,aa,bb,cc,dd,ee,ff,gg", false},
		{"with space token", "a group", false},
		{"empty entries ignored", "a,b,", true},
		{"dedup keeps order", "a,b,a,c", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/list_tables", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set(DefaultPrincipalHeader, "alice")
			req.Header.Set(DefaultGroupsHeader, tc.groups)
			_, apiErr := a.Authenticate(req.Context(), req)
			if tc.wantOK && apiErr != nil {
				t.Fatalf("expected ok, got %v", apiErr)
			}
			if !tc.wantOK && apiErr == nil {
				t.Fatalf("expected error for %q", tc.groups)
			}
		})
	}
}
