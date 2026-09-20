package conformance

import (
	"net/http"
	"strings"
	"testing"
)

func mintKey(t *testing.T, h *harness, name, principal string, groups ...string) (id, secret string) {
	t.Helper()
	req := map[string]any{"name": name, "principal": principal}
	if len(groups) > 0 {
		req["groups"] = groups
	}
	data := h.mustHTTP("create_key", req)
	key, _ := data["key"].(map[string]any)
	id, _ = key["id"].(string)
	secret, _ = data["secret"].(string)
	if id == "" || secret == "" {
		t.Fatalf("create_key returned no id or secret: %v", data)
	}
	return id, secret
}

func TestKeyOpsAreAbsentUnderAuthOff(t *testing.T) {
	h := newHarnessMode(t, authOff)
	for _, op := range []string{"create_key", "list_keys", "revoke_key"} {
		status, out := h.httpCall(op, map[string]any{})
		if status != http.StatusNotFound {
			t.Fatalf("/v1/%s under auth off: status %d, want 404: %v", op, status, out)
		}
	}
	if doc := h.mustHTTPGet(t, "/v1/openapi.json"); strings.Contains(doc, "create_key") {
		t.Fatal("openapi.json advertises create_key under auth off")
	}
}

func TestAKeyAuthenticatesAsItsPrincipal(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	_, secret := mintKey(t, h, "ci runner", "ci-bot", "builders")

	if !strings.HasPrefix(secret, "dlm_") || len(secret) != len("dlm_")+43 {
		t.Fatalf("key shape %q is not dlm_ plus 43 base64url characters", secret)
	}

	status, out := h.httpCallAs(identity{bearer: secret}, "whoami", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("whoami with a key: status %d %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["principal"] != "ci-bot" {
		t.Fatalf("key authenticated as %v, want ci-bot", data["principal"])
	}
	if data["source"] != "api-keys" {
		t.Fatalf("source %v, want api-keys", data["source"])
	}
	groups, _ := data["groups"].([]any)
	if len(groups) != 1 || groups[0] != "builders" {
		t.Fatalf("key groups %v", groups)
	}
}

func TestAKeyGrantsNothingByItself(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	_, secret := mintKey(t, h, "ci runner", "ci-bot")

	status, out := h.httpCallAs(identity{bearer: secret}, "query", map[string]any{"namespace": "acme", "sql": "SELECT 1"})
	if status != http.StatusForbidden {
		t.Fatalf("an ungranted key: status %d, want 403: %v", status, out)
	}

	h.mustHTTP("grant", map[string]any{
		"subject": map[string]any{"type": "principal", "id": "ci-bot"},
		"object":  map[string]any{"namespace": "acme"},
		"verbs":   []string{"read"},
	})
	status, out = h.httpCallAs(identity{bearer: secret}, "query", map[string]any{"namespace": "acme", "sql": "SELECT 1 AS n"})
	if status != http.StatusOK {
		t.Fatalf("a granted key: status %d %v", status, out)
	}
}

func TestGroupGrantsReachKeyIdentities(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	_, secret := mintKey(t, h, "fleet", "bot-7", "builders")
	h.mustHTTP("grant", map[string]any{
		"subject": map[string]any{"type": "group", "id": "builders"},
		"object":  map[string]any{"namespace": "acme"},
		"verbs":   []string{"read"},
	})

	status, out := h.httpCallAs(identity{bearer: secret}, "query", map[string]any{"namespace": "acme", "sql": "SELECT 1 AS n"})
	if status != http.StatusOK {
		t.Fatalf("a group grant did not reach the key identity: status %d %v", status, out)
	}
}

func TestRevokedKeyStopsAuthenticating(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	id, secret := mintKey(t, h, "temp", "temp-bot")

	if status, _ := h.httpCallAs(identity{bearer: secret}, "whoami", map[string]any{}); status != http.StatusOK {
		t.Fatalf("key does not authenticate before revocation: %d", status)
	}
	h.mustHTTP("revoke_key", map[string]any{"id": id})

	status, out := h.httpCallAs(identity{bearer: secret}, "whoami", map[string]any{})
	if status != http.StatusUnauthorized {
		t.Fatalf("a revoked key still authenticates: status %d %v", status, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if errEnv["code"] != "unauthorized" {
		t.Fatalf("a revoked key should be indistinguishable from an unknown one: %v", out)
	}
}

func TestListKeysNeverReturnsCredentials(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	id, secret := mintKey(t, h, "one", "bot-1")

	data := h.mustHTTP("list_keys", map[string]any{})
	raw := mustJSON(t, data)
	if strings.Contains(raw, secret) {
		t.Fatalf("list_keys returned the credential: %s", raw)
	}
	if !strings.Contains(raw, id) || !strings.Contains(raw, "bot-1") {
		t.Fatalf("list_keys omits the key it should report: %s", raw)
	}
}

func TestKeysSharingANameStayIndividuallyRevocable(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	firstID, firstSecret := mintKey(t, h, "runner", "bot")
	_, secondSecret := mintKey(t, h, "runner", "bot")

	h.mustHTTP("revoke_key", map[string]any{"id": firstID})

	if status, _ := h.httpCallAs(identity{bearer: firstSecret}, "whoami", map[string]any{}); status != http.StatusUnauthorized {
		t.Fatalf("the revoked key still authenticates: %d", status)
	}
	if status, _ := h.httpCallAs(identity{bearer: secondSecret}, "whoami", map[string]any{}); status != http.StatusOK {
		t.Fatalf("revoking one key killed its namesake: %d", status)
	}
}

func TestKeyIdentityLimits(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)

	for name, req := range map[string]map[string]any{
		"reserved principal":  {"name": "k", "principal": "dolmen-admin"},
		"space in principal":  {"name": "k", "principal": "bot seven"},
		"missing principal":   {"name": "k"},
		"missing name":        {"principal": "bot"},
		"comma in group":      {"name": "k", "principal": "bot", "groups": []string{"a,b"}},
		"duplicate group":     {"name": "k", "principal": "bot", "groups": []string{"a", "a"}},
		"non ascii principal": {"name": "k", "principal": "botté"},
	} {
		status, out := h.httpCall("create_key", req)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400: %v", name, status, out)
		}
	}
}

func TestMalformedKeyShapeIsRefusedAtDispatch(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	for _, bearer := range []string{"dlm_short", "dlm_" + strings.Repeat("a", 44), "dlm_" + strings.Repeat("!", 43)} {
		status, out := h.httpCallAs(identity{bearer: bearer}, "whoami", map[string]any{})
		if status != http.StatusUnauthorized {
			t.Fatalf("bearer %q: status %d, want 401: %v", bearer, status, out)
		}
	}
}

func TestKeyOpsRequireRootAdmin(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	grantTo(t, h, "principal", "alice", "acme", "", "admin")

	res, out := h.asIdentity(t, "alice", "", "create_key", `{"name":"k","principal":"bot"}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("admin on one namespace minted a key: status %d, want 403: %v", res.StatusCode, out)
	}
}

func TestSourceBlindness(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.seedTable("acme", "docs", []map[string]any{{"name": "title", "type": "string"}})
	grantTo(t, h, "principal", "same-principal", "acme", "docs", "create", "read")

	_, secret := mintKey(t, h, "same", "same-principal")

	viaHeader, headerOut := h.postNoCredential(t, h.httpURL+"/read_rows",
		`{"namespace":"acme","table":"docs","ids":[1]}`,
		map[string]string{"X-Dolmen-Principal": "same-principal"})
	viaKey, keyOut := h.httpCallAs(identity{bearer: secret}, "read_rows", map[string]any{
		"namespace": "acme", "table": "docs", "ids": []int64{1},
	})
	if viaHeader.StatusCode != viaKey {
		t.Fatalf("the same identity behaved differently by source: header %d, key %d", viaHeader.StatusCode, viaKey)
	}
	assertJSONEqual(t, "read_rows across sources", headerOut["data"], keyOut["data"])
}
