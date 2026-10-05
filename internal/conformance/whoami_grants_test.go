package conformance

import (
	"net/http"
	"sort"
	"strings"
	"testing"
)

func TestWhoamiListsTheGrantsTheCallerHolds(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.seedTable("acme", "deals", []map[string]any{{"name": "title", "type": "string"}})
	_, secret := mintKey(t, h, "assistant", "sales-bot", "sales-team")
	h.mustHTTP("grant", map[string]any{"subject": map[string]any{"type": "principal", "id": "sales-bot"}, "object": map[string]any{"namespace": "acme", "table": "deals"}, "verbs": []string{"read"}})
	h.mustHTTP("grant", map[string]any{"subject": map[string]any{"type": "group", "id": "sales-team"}, "object": map[string]any{"namespace": "acme"}, "verbs": []string{"create"}})
	h.mustHTTP("grant", map[string]any{"subject": map[string]any{"type": "group", "id": "finance"}, "object": map[string]any{"namespace": "acme"}, "verbs": []string{"read"}})

	status, out := h.httpCallAs(identity{bearer: secret}, "whoami", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("whoami: status %d %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	grants, _ := data["grants"].([]any)
	var got []string
	for _, g := range grants {
		m, _ := g.(map[string]any)
		subj, _ := m["subject"].(map[string]any)
		obj, _ := m["object"].(map[string]any)
		table, _ := obj["table"].(string)
		got = append(got, subj["id"].(string)+" "+obj["namespace"].(string)+"/"+table+" "+strings.Join(stringList(m["verbs"]), "+"))
	}
	sort.Strings(got)
	want := []string{"sales-bot acme/deals read", "sales-team acme/ create"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("whoami grants = %q, want %q (its own and its groups', nobody else's)", got, want)
	}

	data = h.mustHTTP("whoami", map[string]any{})
	grants, _ = data["grants"].([]any)
	if len(grants) != 1 {
		t.Fatalf("the bootstrap administrator must see its implicit grant on *: %v", data)
	}
	root, _ := grants[0].(map[string]any)
	obj, _ := root["object"].(map[string]any)
	if obj["namespace"] != "*" || !strings.Contains(strings.Join(stringList(root["verbs"]), ","), "admin") {
		t.Fatalf("the bootstrap grant must be admin on *: %v", root)
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
