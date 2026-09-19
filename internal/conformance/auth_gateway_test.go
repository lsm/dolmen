package conformance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func assertForbiddenEnvelope(t *testing.T, what string, res *http.Response, out map[string]any) {
	t.Helper()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("%s: status %d, want 403: %v", what, res.StatusCode, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if errEnv == nil || errEnv["code"] != "forbidden" {
		t.Fatalf("%s: envelope %v, want code forbidden", what, out)
	}
}

func aliceHeaders() map[string]string {
	return map[string]string{"X-Dolmen-Principal": "alice", "X-Dolmen-Groups": "team-a,readers"}
}

func TestGatewayAssertedIdentityAuthenticatesThenIsUngranted(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "gw"})

	res, out := h.postNoCredential(t, h.httpURL+"/query", `{"namespace":"gw","sql":"SELECT 1"}`, aliceHeaders())
	assertForbiddenEnvelope(t, "asserted alice running query", res, out)

	errEnv, _ := out["error"].(map[string]any)
	msg, _ := errEnv["message"].(string)
	for _, leak := range []string{"alice", "team-a", "readers"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("the forbidden message echoes caller-supplied identity %q: %q", leak, msg)
		}
	}
}

func TestGatewayGrantFreeOpsSucceedForAnUngrantedCaller(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "gw"})

	res, out := h.postNoCredential(t, h.httpURL+"/list_namespaces", "{}", aliceHeaders())
	if res.StatusCode != http.StatusOK || out["ok"] != true {
		t.Fatalf("list_namespaces as an ungranted caller: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	nss, _ := data["namespaces"].([]any)
	if len(nss) != 0 {
		t.Fatalf("list_namespaces leaked namespaces to an ungranted caller: %v", nss)
	}

	for _, op := range []string{"describe_server", "capabilities", "whoami"} {
		res, out := h.postNoCredential(t, h.httpURL+"/"+op, "{}", aliceHeaders())
		if res.StatusCode != http.StatusOK || out["ok"] != true {
			t.Fatalf("%s as an ungranted caller: status %d %v", op, res.StatusCode, out)
		}
	}

	res, out = h.postNoCredential(t, h.httpURL+"/list_tables", `{"namespace":"gw"}`, aliceHeaders())
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("list_tables on an unreachable namespace: status %d, want 404 so listing cannot enumerate names: %v", res.StatusCode, out)
	}
}

func TestGatewayAdminKeyStillWorksAlongsideHeaders(t *testing.T) {
	h := newHarnessMode(t, authGateway)

	status, out := h.httpCallAs(identity{
		principal: "alice",
		groups:    []string{"team-a"},
		bearer:    authGateway.adminKey,
	}, "list_namespaces", map[string]any{})
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("admin key alongside asserted headers: status %d %v", status, out)
	}
}

func TestGatewayRejectsMalformedAndReservedAssertions(t *testing.T) {
	h := newHarnessMode(t, authGateway)

	for name, hdr := range map[string]map[string]string{
		"reserved principal":  {"X-Dolmen-Principal": "dolmen-admin"},
		"space in principal":  {"X-Dolmen-Principal": "alice smith"},
		"non ascii principal": {"X-Dolmen-Principal": "alicé"},
		"long principal":      {"X-Dolmen-Principal": strings.Repeat("a", 257)},
		"space in group":      {"X-Dolmen-Principal": "alice", "X-Dolmen-Groups": "team a"},
		"groups only":         {"X-Dolmen-Groups": "team-a"},
	} {
		res, out := h.postNoCredential(t, h.httpURL+"/list_namespaces", "{}", hdr)
		assertUnauthorizedEnvelope(t, name, res, out)
	}
}

func TestGatewayOverLimitGroupsFailTheIdentity(t *testing.T) {
	mode := authGateway
	mode.maxGroups = 2
	h := newHarnessMode(t, mode)

	res, out := h.postNoCredential(t, h.httpURL+"/list_namespaces", "{}", map[string]string{
		"X-Dolmen-Principal": "alice",
		"X-Dolmen-Groups":    "a,b",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("at the group limit: status %d, want 200: %v", res.StatusCode, out)
	}

	res, out = h.postNoCredential(t, h.httpURL+"/list_namespaces", "{}", map[string]string{
		"X-Dolmen-Principal": "alice",
		"X-Dolmen-Groups":    "a,b,c",
	})
	assertUnauthorizedEnvelope(t, "over the group limit", res, out)
}

func TestGatewayInvalidBearerNeverDowngradesToHeaders(t *testing.T) {
	h := newHarnessMode(t, authGateway)

	res, out := h.postNoCredential(t, h.httpURL+"/list_namespaces", "{}", map[string]string{
		"X-Dolmen-Principal": "alice",
		"Authorization":      "Bearer wrong-but-well-shaped-credential-0000",
	})
	assertUnauthorizedEnvelope(t, "invalid bearer with asserted headers", res, out)
}

func TestGatewayIdentityReachesMCPAndSubscribe(t *testing.T) {
	h := newHarnessMode(t, authGateway)

	h.mustHTTP("create_namespace", map[string]any{"namespace": "gw"})

	res, out := h.postNoCredential(t, h.mcpURL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"namespace":"gw","sql":"SELECT 1"}}}`,
		map[string]string{"X-Dolmen-Principal": "alice"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/mcp as alice: status %d, want a tool error inside 200: %v", res.StatusCode, out)
	}
	if !strings.Contains(mustJSON(t, out), "forbidden") {
		t.Fatalf("/mcp query as an ungranted caller was not refused: %v", out)
	}

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/subscribe?namespace=gw", nil)
	if err != nil {
		t.Fatalf("new subscribe request: %v", err)
	}
	req.Header.Set("X-Dolmen-Principal", "alice")
	sub, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Body.Close()
	if sub.StatusCode != http.StatusForbidden {
		t.Fatalf("/v1/subscribe as alice: status %d, want 403", sub.StatusCode)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
