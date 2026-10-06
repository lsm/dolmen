package conformance

import (
	"net/http"
	"testing"
	"time"
)

func TestCreateKeyTakesAnExpiryAndListKeysReportsIt(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	data := h.mustHTTP("create_key", map[string]any{"name": "contractor", "principal": "temp", "expires_at": until.Format(time.RFC3339)})
	key, _ := data["key"].(map[string]any)
	if got, _ := time.Parse(time.RFC3339Nano, key["expires_at"].(string)); !got.Equal(until) || key["expired"] != false {
		t.Fatalf("create_key must echo expires_at %v and expired false: %v", until, key)
	}
	revokedID, _ := mintKey(t, h, "old", "old-bot")
	h.mustHTTP("revoke_key", map[string]any{"id": revokedID})

	for _, past := range []string{time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "next tuesday"} {
		status, out := h.httpCall("create_key", map[string]any{"name": "bad", "principal": "bad-bot", "expires_at": past})
		if status != http.StatusBadRequest {
			t.Fatalf("expires_at %q: status %d, want 400: %v", past, status, out)
		}
	}

	all := h.mustHTTP("list_keys", map[string]any{})
	active := h.mustHTTP("list_keys", map[string]any{"active_only": true})
	if n := len(all["keys"].([]any)); n != 2 {
		t.Fatalf("list_keys must keep revoked keys by default, got %d: %v", n, all)
	}
	keys := active["keys"].([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["name"] != "contractor" {
		t.Fatalf("list_keys active_only must drop revoked and expired keys: %v", active)
	}
}
