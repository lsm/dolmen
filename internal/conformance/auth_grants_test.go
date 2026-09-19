package conformance

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/auth"
)

func (h *harness) mustHTTPGet(t *testing.T, path string) string {
	t.Helper()
	res, body := h.getNoCredential(t, path)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get %s: status %d: %s", path, res.StatusCode, body)
	}
	return body
}

func (h *harness) mcpRaw(t *testing.T, body string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.mcpURL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new mcp request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.mode.on() {
		req.Header.Set("Authorization", "Bearer "+h.mode.adminKey)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post mcp: %v", err)
	}
	defer res.Body.Close()
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read mcp body: %v", err)
	}
	return string(buf)
}

func (h *harness) asAlice(t *testing.T, op, body string) (*http.Response, map[string]any) {
	t.Helper()
	return h.postNoCredential(t, h.httpURL+"/"+op, body, map[string]string{"X-Dolmen-Principal": "alice"})
}

func (h *harness) asIdentity(t *testing.T, principal, groups, op, body string) (*http.Response, map[string]any) {
	t.Helper()
	hdr := map[string]string{"X-Dolmen-Principal": principal}
	if groups != "" {
		hdr["X-Dolmen-Groups"] = groups
	}
	return h.postNoCredential(t, h.httpURL+"/"+op, body, hdr)
}

func grantTo(t *testing.T, h *harness, subjType, subjID, ns, table string, verbs ...string) {
	t.Helper()
	obj := map[string]any{"namespace": ns}
	if table != "" {
		obj["table"] = table
	}
	h.mustHTTP("grant", map[string]any{
		"subject": map[string]any{"type": subjType, "id": subjID},
		"object":  obj,
		"verbs":   verbs,
	})
}

func seedGrantHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.seedTable("acme", "docs", []map[string]any{{"name": "title", "type": "string"}})
	return h
}

func TestGrantOpsAreAbsentUnderAuthOff(t *testing.T) {
	h := newHarnessMode(t, authOff)

	for _, op := range []string{"grant", "revoke", "list_grants", "whoami"} {
		status, out := h.httpCall(op, map[string]any{})
		if status != http.StatusNotFound {
			t.Fatalf("/v1/%s under auth off: status %d, want 404: %v", op, status, out)
		}
		res := h.mcpCall(op, map[string]any{})
		if res.proto == nil {
			t.Fatalf("%s over MCP under auth off was dispatched: %+v", op, res)
		}
	}

	doc := h.mustHTTPGet(t, "/v1/openapi.json")
	for _, op := range []string{"/v1/grant", "/v1/revoke", "/v1/list_grants", "/v1/whoami"} {
		if strings.Contains(doc, op) {
			t.Fatalf("openapi.json under auth off advertises %s", op)
		}
	}

	res := h.mcpRaw(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, op := range []string{"\"grant\"", "\"revoke\"", "\"list_grants\"", "\"whoami\""} {
		if strings.Contains(res, op) {
			t.Fatalf("tools/list under auth off advertises %s", op)
		}
	}
}

func TestGrantOpsArePresentUnderAuthOn(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)

	doc := h.mustHTTPGet(t, "/v1/openapi.json")
	for _, op := range []string{"/v1/grant", "/v1/revoke", "/v1/list_grants", "/v1/whoami"} {
		if !strings.Contains(doc, op) {
			t.Fatalf("openapi.json under auth on omits %s", op)
		}
	}
	res := h.mcpRaw(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, op := range []string{"\"grant\"", "\"revoke\"", "\"list_grants\"", "\"whoami\""} {
		if !strings.Contains(res, op) {
			t.Fatalf("tools/list under auth on omits %s", op)
		}
	}
}

func TestGrantUnlocksExactlyTheGrantedVerbs(t *testing.T) {
	h := seedGrantHarness(t)

	res, out := h.asAlice(t, "insert", `{"namespace":"acme","table":"docs","records":[{"title":"x"}]}`)
	assertForbiddenEnvelope(t, "insert before any grant", res, out)

	grantTo(t, h, "principal", "alice", "acme", "docs", "create")

	res, out = h.asAlice(t, "insert", `{"namespace":"acme","table":"docs","records":[{"title":"x"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("insert after a create grant: status %d %v", res.StatusCode, out)
	}

	res, out = h.asAlice(t, "search_fulltext", `{"namespace":"acme","table":"docs","query":"x"}`)
	assertForbiddenEnvelope(t, "search with create only", res, out)

	res, out = h.asAlice(t, "update", `{"namespace":"acme","table":"docs","id":1,"values":{"title":"y"}}`)
	assertForbiddenEnvelope(t, "update with create only", res, out)

	res, out = h.asAlice(t, "delete", `{"namespace":"acme","table":"docs","ids":[1]}`)
	assertForbiddenEnvelope(t, "delete with create only", res, out)
}

func TestUpsertNeedsBothCreateAndUpdate(t *testing.T) {
	h := seedGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "docs", "create")

	res, out := h.asAlice(t, "upsert", `{"namespace":"acme","table":"docs","filter":"title = 'x'","set":{"title":"x"}}`)
	assertForbiddenEnvelope(t, "upsert with create only", res, out)

	res, out = h.asAlice(t, "upsert_by_key", `{"namespace":"acme","table":"docs","key":["title"],"record":{"title":"x"}}`)
	assertForbiddenEnvelope(t, "upsert_by_key with create only", res, out)

	grantTo(t, h, "principal", "alice", "acme", "docs", "update")
	res, out = h.asAlice(t, "upsert", `{"namespace":"acme","table":"docs","filter":"title = 'x'","set":{"title":"x"}}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("upsert with create and update: status %d %v", res.StatusCode, out)
	}
}

func TestQueryRequiresNamespaceWideRead(t *testing.T) {
	h := seedGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "docs", "read")

	res, out := h.asAlice(t, "query", `{"namespace":"acme","sql":"SELECT 1"}`)
	assertForbiddenEnvelope(t, "query with a table-only read grant", res, out)

	grantTo(t, h, "principal", "alice", "acme", "", "read")
	res, out = h.asAlice(t, "query", `{"namespace":"acme","sql":"SELECT 1 AS n"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("query with namespace read: status %d %v", res.StatusCode, out)
	}
}

func TestGroupGrantsWork(t *testing.T) {
	h := seedGrantHarness(t)
	grantTo(t, h, "group", "readers", "acme", "", "read")

	res, out := h.asIdentity(t, "bob", "readers", "read_rows", `{"namespace":"acme","table":"docs","ids":[1]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("group grant did not authorize bob: status %d %v", res.StatusCode, out)
	}

	res, out = h.asIdentity(t, "carol", "others", "read_rows", `{"namespace":"acme","table":"docs","ids":[1]}`)
	assertForbiddenEnvelope(t, "a different group", res, out)
}

func TestDropTableNeedsAdminNotJustSchema(t *testing.T) {
	h := seedGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "docs", "schema")

	res, out := h.asAlice(t, "drop_table", `{"namespace":"acme","table":"docs","confirm":"docs"}`)
	assertForbiddenEnvelope(t, "drop_table with schema only", res, out)

	grantTo(t, h, "principal", "alice", "acme", "docs", "admin")
	res, out = h.asAlice(t, "drop_table", `{"namespace":"acme","table":"docs","confirm":"docs"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("drop_table with schema and admin: status %d %v", res.StatusCode, out)
	}
}

func TestCreateNamespaceChecksTheParent(t *testing.T) {
	h := seedGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "", "admin")

	res, out := h.asAlice(t, "create_namespace", `{"namespace":"acme/team-a"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create_namespace under a granted parent: status %d %v", res.StatusCode, out)
	}

	res, out = h.asAlice(t, "create_namespace", `{"namespace":"elsewhere"}`)
	assertForbiddenEnvelope(t, "create_namespace at the root", res, out)
}

func TestImplicitNamespaceCreationIsOffUnderAuthOn(t *testing.T) {
	h := seedGrantHarness(t)
	grantTo(t, h, "principal", "alice", "*", "", "create", "schema")

	res, out := h.asAlice(t, "create_table", `{"namespace":"ghost","table":"t","fields":[{"name":"a","type":"string"}]}`)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("create_table on a missing namespace: status %d, want 404: %v", res.StatusCode, out)
	}
	status, _ := h.httpCall("list_namespaces", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("list_namespaces: %d", status)
	}
	data := h.mustHTTP("list_namespaces", map[string]any{})
	for _, ns := range data["namespaces"].([]any) {
		if ns == "ghost" {
			t.Fatal("a refused write still created the namespace")
		}
	}
}

func TestGrantsTargetExistingObjectsOnly(t *testing.T) {
	h := seedGrantHarness(t)

	status, out := h.httpCall("grant", map[string]any{
		"subject": map[string]any{"type": "principal", "id": "alice"},
		"object":  map[string]any{"namespace": "nope"},
		"verbs":   []string{"read"},
	})
	if status != http.StatusNotFound {
		t.Fatalf("grant on a missing namespace: status %d, want 404: %v", status, out)
	}

	status, out = h.httpCall("grant", map[string]any{
		"subject": map[string]any{"type": "principal", "id": "alice"},
		"object":  map[string]any{"namespace": "acme", "table": "nope"},
		"verbs":   []string{"read"},
	})
	if status != http.StatusNotFound {
		t.Fatalf("grant on a missing table: status %d, want 404: %v", status, out)
	}

	status, out = h.httpCall("grant", map[string]any{
		"subject": map[string]any{"type": "principal", "id": "alice"},
		"object":  map[string]any{"namespace": "*", "table": "docs"},
		"verbs":   []string{"read"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("a table under the root wildcard: status %d, want 400: %v", status, out)
	}
}

func TestDroppingAnObjectRemovesItsGrants(t *testing.T) {
	h := seedGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "docs", "read")

	data := h.mustHTTP("list_grants", map[string]any{})
	if len(data["grants"].([]any)) != 1 {
		t.Fatalf("expected one grant: %v", data)
	}

	h.mustHTTP("drop_table", map[string]any{"namespace": "acme", "table": "docs", "confirm": "docs"})
	data = h.mustHTTP("list_grants", map[string]any{})
	if n := len(data["grants"].([]any)); n != 0 {
		t.Fatalf("dropping the table left %d grants targeting it: %v", n, data)
	}
}

func TestWhoamiReportsTheAuthenticatedIdentity(t *testing.T) {
	h := seedGrantHarness(t)

	res, out := h.asIdentity(t, "alice", "team-a,readers", "whoami", "{}")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("whoami: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["principal"] != "alice" {
		t.Fatalf("whoami principal %v", data["principal"])
	}
	groups, _ := data["groups"].([]any)
	if len(groups) != 2 || groups[0] != "team-a" || groups[1] != "readers" {
		t.Fatalf("whoami groups %v", groups)
	}
	if data["source"] != "trusted-proxy" {
		t.Fatalf("whoami source %v", data["source"])
	}
}

func TestLastRootAdminGuard(t *testing.T) {
	h := newHarnessMode(t, authGatewayNoKey)
	ctx := h.grants

	if _, err := ctx.Grant(t.Context(), auth.Subject{Type: auth.SubjectPrincipal, ID: "root"},
		auth.Object{Namespace: auth.RootObject}, auth.NewVerbSet(auth.VerbAdmin)); err != nil {
		t.Fatalf("seed root grant: %v", err)
	}

	res, out := h.asIdentity(t, "root", "", "grant", `{"subject":{"type":"principal","id":"second"},"object":{"namespace":"*"},"verbs":["admin"]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("granting a second root administrator: status %d %v", res.StatusCode, out)
	}

	res, out = h.asIdentity(t, "root", "", "revoke", `{"subject":{"type":"principal","id":"second"},"object":{"namespace":"*"},"verbs":["admin"]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("revoking one of two root administrators: status %d %v", res.StatusCode, out)
	}

	res, out = h.asIdentity(t, "root", "", "revoke", `{"subject":{"type":"principal","id":"root"},"object":{"namespace":"*"},"verbs":["admin"]}`)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("revoking the last root administrator: status %d, want 409: %v", res.StatusCode, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "DOLMEN_ADMIN_KEY") {
		t.Fatalf("the refusal does not name the recovery: %v", out)
	}
}

func TestAdminKeyLetsTheLastRootGrantGo(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("grant", map[string]any{
		"subject": map[string]any{"type": "principal", "id": "root"},
		"object":  map[string]any{"namespace": "*"},
		"verbs":   []string{"admin"},
	})
	data := h.mustHTTP("revoke", map[string]any{
		"subject": map[string]any{"type": "principal", "id": "root"},
		"object":  map[string]any{"namespace": "*"},
		"verbs":   []string{"admin"},
	})
	if data["grant"] != nil {
		t.Fatalf("revoking the last verb left a grant: %v", data)
	}
}
