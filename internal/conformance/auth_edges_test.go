package conformance

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAnUnauthenticatedRequestHearsUnauthorizedWhateverItsContentType(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	plain := map[string]string{"Content-Type": "text/plain"}
	res, out := h.postNoCredential(t, h.httpURL+"/list_namespaces", "{}", plain)
	assertUnauthorizedEnvelope(t, "/v1 with a text/plain body and no credential", res, out)
	res, out = h.postNoCredential(t, h.mcpURL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, plain)
	assertUnauthorizedEnvelope(t, "/mcp with a text/plain body and no credential", res, out)

	for _, url := range []string{h.httpURL + "/list_namespaces", h.mcpURL} {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Authorization", "Bearer "+h.mode.adminKey)
		got, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got.Body.Close()
		if got.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("%s with a credential and a text/plain body: status %d, want 415", url, got.StatusCode)
		}
	}
}

func TestRevokeSaysWhatItRemoved(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	subject := map[string]any{"type": "group", "id": "team-a"}
	object := map[string]any{"namespace": "acme"}
	h.mustHTTP("grant", map[string]any{"subject": subject, "object": object, "verbs": []string{"read", "create"}})

	data := h.mustHTTP("revoke", map[string]any{"subject": subject, "object": object, "verbs": []string{"read", "delete"}})
	if got := strings.Join(stringList(data["removed"]), ","); got != "read" {
		t.Fatalf("revoking read and delete from read+create removed %q, want read: %v", got, data)
	}
	data = h.mustHTTP("revoke", map[string]any{"subject": subject, "object": object, "verbs": []string{"create"}})
	if data["grant"] != nil || strings.Join(stringList(data["removed"]), ",") != "create" {
		t.Fatalf("revoking the last verb must say it removed create and left no grant: %v", data)
	}
	data = h.mustHTTP("revoke", map[string]any{"subject": subject, "object": object, "verbs": []string{"create"}})
	if data["grant"] != nil || len(stringList(data["removed"])) != 0 || data["removed"] == nil {
		t.Fatalf("revoking a grant that no longer exists must report an empty removed list: %v", data)
	}

	id, _ := mintKey(t, h, "temp", "temp-bot")
	if data := h.mustHTTP("revoke_key", map[string]any{"id": id}); data["changed"] != true {
		t.Fatalf("the first revoke_key must report changed: %v", data)
	}
	if data := h.mustHTTP("revoke_key", map[string]any{"id": id}); data["changed"] != false {
		t.Fatalf("revoking an already-revoked key must report changed false: %v", data)
	}
	if data := h.mustHTTP("revoke_key", map[string]any{"id": "no-such-key"}); data["key"] != nil || data["changed"] != false {
		t.Fatalf("revoking an unknown key must report no key and changed false: %v", data)
	}
}

func TestGrantSaysWhetherAnyoneCanAuthenticateAsItsSubject(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	mintKey(t, h, "ci", "ci-bot", "builders")
	object := map[string]any{"namespace": "acme"}
	for _, tc := range []struct {
		subject map[string]any
		want    bool
	}{
		{map[string]any{"type": "principal", "id": "ci-bot"}, true},
		{map[string]any{"type": "group", "id": "builders"}, true},
		{map[string]any{"type": "principal", "id": "ci-bto"}, false},
		{map[string]any{"type": "group", "id": "bulders"}, false},
	} {
		data := h.mustHTTP("grant", map[string]any{"subject": tc.subject, "object": object, "verbs": []string{"read"}})
		if data["reachable"] != tc.want {
			t.Fatalf("grant to %v: reachable = %v, want %v", tc.subject, data["reachable"], tc.want)
		}
	}

	g := newHarnessMode(t, authGateway)
	g.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	data := g.mustHTTP("grant", map[string]any{"subject": map[string]any{"type": "principal", "id": "alice"}, "object": object, "verbs": []string{"read"}})
	if data["reachable"] != true {
		t.Fatalf("behind a gateway any principal can be asserted, so a grant to it is reachable: %v", data)
	}
}

func TestToolsListHidesServerWideAdminToolsFromOthers(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	_, secret := mintKey(t, h, "reader", "reader-bot")
	rootOnly := []string{"create_key", "list_keys", "revoke_key", "rotate_secret_key"}

	names := func(bearer string) map[string]bool {
		req, err := http.NewRequest(http.MethodPost, h.mcpURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var out struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("tools/list: %v: %s", err, raw)
		}
		got := map[string]bool{}
		for _, tool := range out.Result.Tools {
			got[tool.Name] = true
		}
		return got
	}

	admin, reader := names(h.mode.adminKey), names(secret)
	for _, name := range rootOnly {
		if !admin[name] {
			t.Fatalf("a root administrator must still see %s", name)
		}
		if reader[name] {
			t.Fatalf("a caller without admin on * sees %s, which can only ever answer 403 for them", name)
		}
	}
	if !reader["whoami"] || !reader["read_rows"] {
		t.Fatalf("tools/list must keep the tools a non-admin can use: %v", reader)
	}
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
