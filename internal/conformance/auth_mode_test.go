package conformance

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/api"
)

func sortedOpNames() []string {
	names := make([]string, 0, len(api.Ops))
	for name := range api.Ops {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (h *harness) getNoCredential(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	res, err := http.Get(h.srv.URL + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer res.Body.Close()
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return res, string(buf)
}

func (h *harness) postNoCredential(t *testing.T, url, body string, hdr ...map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range hdr {
		for k, v := range h {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s response: %v", url, err)
	}
	return res, out
}

func assertUnauthorizedEnvelope(t *testing.T, what string, res *http.Response, out map[string]any) {
	t.Helper()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("%s: status %d, want 401: %v", what, res.StatusCode, out)
	}
	if out["ok"] != false {
		t.Fatalf("%s: envelope ok is not false: %v", what, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if errEnv == nil {
		t.Fatalf("%s: no error envelope: %v", what, out)
	}
	if errEnv["code"] != "unauthorized" {
		t.Fatalf("%s: error code %v, want unauthorized", what, errEnv["code"])
	}
	if msg, _ := errEnv["message"].(string); msg == "" {
		t.Fatalf("%s: error carries no message: %v", what, out)
	}
	if errEnv["request_id"] == "" {
		t.Fatalf("%s: error carries no request_id: %v", what, out)
	}
}

func TestAuthOnBootsAndServesWithAdminKey(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)

	ns := "authon"
	h.mustHTTP("create_namespace", map[string]any{"namespace": ns})
	h.seedTable(ns, "docs", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": ns, "table": "docs",
		"records": []map[string]any{{"title": "hello"}},
	})
	data := h.mustHTTP("query", map[string]any{"namespace": ns, "sql": "SELECT id, title FROM docs"})
	if got := int64val(t, "row_count", data["row_count"]); got != 1 {
		t.Fatalf("row_count %d, want 1: %v", got, data)
	}

	res := h.mcpCall("list_tables", map[string]any{"namespace": ns})
	if res.status != http.StatusOK || res.proto != nil || res.isError() {
		t.Fatalf("list_tables over MCP with the admin key failed: %+v", res)
	}
}

func TestAuthOnDeniesEveryOpWithoutCredential(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)

	for _, op := range sortedOpNames() {
		res, out := h.postNoCredential(t, h.httpURL+"/"+op, "{}")
		assertUnauthorizedEnvelope(t, "/v1/"+op, res, out)
	}

	res, out := h.postNoCredential(t, h.mcpURL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertUnauthorizedEnvelope(t, "/mcp tools/list", res, out)

	res, out = h.postNoCredential(t, h.mcpURL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_namespaces","arguments":{}}}`)
	assertUnauthorizedEnvelope(t, "/mcp tools/call", res, out)

	sub, body := h.getNoCredential(t, "/v1/subscribe?namespace=authon")
	if sub.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1/subscribe: status %d, want 401: %s", sub.StatusCode, body)
	}
	if !strings.Contains(body, `"unauthorized"`) {
		t.Fatalf("/v1/subscribe: body does not carry the unauthorized code: %s", body)
	}
}

func TestAuthOnRejectsWrongCredentialsUniformly(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)

	var seen []string
	for _, bearer := range []string{
		"wrong-but-well-shaped-credential-000000000",
		"dlm_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"a.b.c",
		authAdminKey.adminKey + "x",
	} {
		status, out := h.httpCallAs(identity{bearer: bearer}, "list_namespaces", map[string]any{})
		if status != http.StatusUnauthorized {
			t.Fatalf("bearer %q: status %d, want 401: %v", bearer, status, out)
		}
		errEnv, _ := out["error"].(map[string]any)
		if errEnv == nil || errEnv["code"] != "unauthorized" {
			t.Fatalf("bearer %q: envelope %v", bearer, out)
		}
		msg, _ := errEnv["message"].(string)
		seen = append(seen, msg)
	}
	for i, m := range seen {
		if m != seen[0] {
			t.Fatalf("rejection %d differs, so the message distinguishes failures:\n%q\n%q", i, m, seen[0])
		}
	}
}

func TestAuthOnIgnoresAssertedIdentityHeaders(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)

	req, err := http.NewRequest(http.MethodPost, h.httpURL+"/list_namespaces", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dolmen-Principal", "alice")
	req.Header.Set("X-Dolmen-Groups", "admins")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post list_namespaces: %v", err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	assertUnauthorizedEnvelope(t, "asserted identity headers", res, out)
}

func TestAuthOnLeavesDiscoveryPathsUnauthenticated(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)

	for _, path := range []string{"/healthz", "/version", "/skills", "/skills/dolmen", "/v1/openapi.json"} {
		res, body := h.getNoCredential(t, path)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s without a credential: status %d, want 200: %s", path, res.StatusCode, body)
		}
		if strings.Contains(body, authAdminKey.adminKey) {
			t.Fatalf("%s echoes the admin key", path)
		}
	}
}

func TestAuthOffNeverEmitsUnauthorized(t *testing.T) {
	h := newHarnessMode(t, authOff)

	for _, bearer := range []string{"garbage", "dlm_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", authAdminKey.adminKey} {
		status, out := h.httpCallAs(identity{bearer: bearer}, "list_namespaces", map[string]any{})
		if status != http.StatusOK || out["ok"] != true {
			t.Fatalf("auth off rejected bearer %q: status %d %v", bearer, status, out)
		}
	}

	for _, op := range sortedOpNames() {
		res, out := h.postNoCredential(t, h.httpURL+"/"+op, "{}")
		if res.StatusCode == http.StatusUnauthorized {
			t.Fatalf("/v1/%s answered 401 under auth off: %v", op, out)
		}
		if errEnv, _ := out["error"].(map[string]any); errEnv != nil && errEnv["code"] == "unauthorized" {
			t.Fatalf("/v1/%s emitted the unauthorized code under auth off: %v", op, out)
		}
	}
}

func openAPIErrorCodes(t *testing.T, h *harness) []any {
	t.Helper()
	res, body := h.getNoCredential(t, "/v1/openapi.json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/v1/openapi.json: status %d", res.StatusCode)
	}
	var doc struct {
		Components struct {
			Schemas struct {
				ErrorEnvelope struct {
					Properties struct {
						Error struct {
							Properties struct {
								Code struct {
									Enum []any `json:"enum"`
								} `json:"code"`
							} `json:"properties"`
						} `json:"error"`
					} `json:"properties"`
				} `json:"ErrorEnvelope"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode /v1/openapi.json: %v", err)
	}
	return doc.Components.Schemas.ErrorEnvelope.Properties.Error.Properties.Code.Enum
}

func TestTheOpenAPIErrorEnumNamesUnauthorizedExactlyWhenAuthIsOn(t *testing.T) {
	for _, tc := range []struct {
		mode harnessMode
		want bool
	}{{authOff, false}, {authAdminKey, true}} {
		named := false
		for _, code := range openAPIErrorCodes(t, newHarnessMode(t, tc.mode)) {
			if code == "unauthorized" {
				named = true
			}
		}
		if named != tc.want {
			t.Errorf("auth %s: the error envelope's code enum names unauthorized: %v, want %v; a client validating a real 401 against the published schema must find its code there, and auth off never emits it", tc.mode.name, named, tc.want)
		}
	}
}
