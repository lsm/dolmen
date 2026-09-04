package conformance

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/lsm/dolmen/internal/api"
)

func authOn(t *testing.T) *api.Auth {
	t.Helper()
	a, err := api.NewAuth("on", "127.0.0.1/32,::1/128", "", "", "")
	if err != nil {
		t.Fatalf("auth on: %v", err)
	}
	return a
}

func TestAuthOffIgnoresHeaders(t *testing.T) {
	h := newHarness(t)
	status, out := h.httpCallWithHeaders("list_namespaces", map[string]any{}, map[string]string{
		api.DefaultPrincipalHeader: "eve",
	})
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("auth off must accept requests with credentials: %d %v", status, out)
	}
}

func TestAuthOnRequiresPrincipal(t *testing.T) {
	h := newHarnessWithAuth(t, authOn(t))
	status, out := h.httpCall("list_namespaces", map[string]any{})
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", status)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != string(api.ErrCodeUnauthorized) {
		t.Fatalf("expected code %q, got %v", api.ErrCodeUnauthorized, errObj["code"])
	}
	if errObj["request_id"] == "" {
		t.Fatalf("error must carry request_id: %v", errObj)
	}
}

func TestAuthOnAcceptsTrustedProxy(t *testing.T) {
	h := newHarnessWithAuth(t, authOn(t))
	status, out := h.httpCallWithHeaders("list_namespaces", map[string]any{}, map[string]string{
		api.DefaultPrincipalHeader: "alice",
	})
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("trusted proxy call failed: %d %v", status, out)
	}
}

func TestAuthOnRejectsUntrustedProxy(t *testing.T) {
	a, err := api.NewAuth("on", "10.0.0.0/8", "", "", "")
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}
	h := newHarnessWithAuth(t, a)
	status, out := h.httpCallWithHeaders("list_namespaces", map[string]any{}, map[string]string{
		api.DefaultPrincipalHeader: "alice",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 from untrusted proxy, got %d %v", status, out)
	}
}

func TestAuthOnRejectsMalformedPrincipal(t *testing.T) {
	h := newHarnessWithAuth(t, authOn(t))
	for _, bad := range []string{"has space", "has\tnonprint", ""} {
		status, out := h.httpCallWithHeaders("list_namespaces", map[string]any{}, map[string]string{
			api.DefaultPrincipalHeader: bad,
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("principal %q: expected 401, got %d %v", bad, status, out)
		}
	}
}

func TestAuthOnRejectsReservedAdminPrincipal(t *testing.T) {
	h := newHarnessWithAuth(t, authOn(t))
	status, out := h.httpCallWithHeaders("list_namespaces", map[string]any{}, map[string]string{
		api.DefaultPrincipalHeader: api.BuiltinAdminPrincipal,
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("reserved principal: expected 401, got %d %v", status, out)
	}
}

func TestAuthOnAdminKeyBypassesProxy(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef"
	a, err := api.NewAuth("on", "", "", "", key)
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}
	h := newHarnessWithAuth(t, a)
	status, out := h.httpCallWithHeaders("list_namespaces", map[string]any{}, map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", key),
	})
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("admin key call failed: %d %v", status, out)
	}
}

func TestAuthOnPublicEndpointsRemainOpen(t *testing.T) {
	h := newHarnessWithAuth(t, authOn(t))
	for _, path := range []string{"/healthz", "/version", "/skills", "/v1/openapi.json"} {
		res, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: expected public success, got %d", path, res.StatusCode)
		}
	}
}

func TestAuthOnMCPRequiresIdentity(t *testing.T) {
	h := newHarnessWithAuth(t, authOn(t))
	res := h.mcpCall("list_namespaces", map[string]any{})
	if res.status != http.StatusOK || res.result == nil || !res.isError() {
		t.Fatalf("expected MCP tool error, got %+v", res)
	}
	errEnv := res.toolError()
	if errEnv == nil {
		t.Fatalf("expected error envelope in tool result")
	}
	if errEnv["code"] != string(api.ErrCodeUnauthorized) {
		t.Fatalf("expected code %q, got %v", api.ErrCodeUnauthorized, errEnv["code"])
	}
}

func TestAuthOnMCPSucceedsWithIdentity(t *testing.T) {
	h := newHarnessWithAuth(t, authOn(t))
	res := h.mcpCallWithHeaders("list_namespaces", map[string]any{}, map[string]string{
		api.DefaultPrincipalHeader: "alice",
	})
	if res.status != http.StatusOK || res.result == nil || res.isError() {
		t.Fatalf("expected MCP success, got %+v", res)
	}
}
