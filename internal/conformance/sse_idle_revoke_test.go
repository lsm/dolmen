package conformance

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
)

func idleRevokeHarness(t *testing.T) *harness {
	t.Helper()
	h := seedRowAccess(t)
	h.apiOpts = []api.Option{api.WithKeepaliveInterval(100 * time.Millisecond)}
	h.reopen()
	return h
}

func wantRevokedClose(t *testing.T, stream *sseReader) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frame, ok := stream.next(time.Until(deadline))
		if !ok {
			break
		}
		if frame.event == "error" {
			if !strings.Contains(frame.data, "subscription authorization was revoked") {
				t.Fatalf("the stream ended with the wrong error: %s", frame.data)
			}
			if !strings.Contains(frame.data, `"code":"forbidden"`) {
				t.Fatalf("a revoked subscription must end as forbidden, not as a malformed request: %s", frame.data)
			}
			return
		}
	}
	t.Fatal("an idle stream stayed open after its caller lost access; the keepalive tick must recheck authorization, not wait for the next commit")
}

func TestAnIdleSubscriptionEndsWhenItsGrantIsRevoked(t *testing.T) {
	h := idleRevokeHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	stream := h.subscribeAs(t, "alice", url.Values{"namespace": {"acme"}, "table": {"notes"}})
	ready, ok := stream.next(5 * time.Second)
	if !ok {
		t.Fatal("the stream never opened")
	}
	wantReady(t, ready)
	h.mustHTTP("revoke", map[string]any{
		"subject": map[string]any{"type": "principal", "id": "alice"},
		"object":  map[string]any{"namespace": "acme", "table": "notes"},
		"verbs":   []string{"create"},
	})
	wantRevokedClose(t, stream)
}

func TestAnIdleSubscriptionEndsWhenItsKeyIsRevoked(t *testing.T) {
	h := idleRevokeHarness(t)
	grantTo(t, h, "principal", "carol", "acme", "notes", "read")
	key := h.mustHTTP("create_key", map[string]any{"name": "carol-feed", "principal": "carol"})
	secret, _ := key["secret"].(string)
	id, _ := key["key"].(map[string]any)["id"].(string)
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/subscribe?namespace=acme&table=notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("subscribe with carol's key: %d", res.StatusCode)
	}
	stream := newSSEReader(res)
	ready, ok := stream.next(5 * time.Second)
	if !ok {
		t.Fatal("the stream never opened")
	}
	wantReady(t, ready)
	h.mustHTTP("revoke_key", map[string]any{"id": id})
	wantRevokedClose(t, stream)
}
