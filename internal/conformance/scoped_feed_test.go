package conformance

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func (h *harness) subscribeAs(t *testing.T, principal string, query url.Values) *sseReader {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/subscribe?"+query.Encode(), nil)
	if err != nil {
		t.Fatalf("build subscribe request: %v", err)
	}
	req.Header.Set("X-Dolmen-Principal", principal)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open subscribe stream as %s: %v", principal, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("subscribe as %s: status %d", principal, res.StatusCode)
	}
	return newSSEReader(res)
}

func TestAScopedSubscriberReceivesItsOwnWritesAndNoOthers(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")

	stream := h.subscribeAs(t, "alice", url.Values{"namespace": {"acme"}, "table": {"notes"}})
	ready, ok := stream.next(5 * time.Second)
	if !ok {
		t.Fatal("the scoped stream never opened")
	}
	wantReady(t, ready)

	res, out := h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"from alice"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice insert: %d %v", res.StatusCode, out)
	}
	frame, ok := stream.next(5 * time.Second)
	if !ok {
		t.Fatal("alice received no event for her own write; the change record carries the owner label the live feed filters on, so an unlabelled record is invisible to its own writer")
	}
	got := sseChangeOf(t, frame)
	if got[0] != "notes" || got[2] != "insert" {
		t.Fatalf("unexpected first event: %v", got)
	}

	res, out = h.asIdentity(t, "bob", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"from bob"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bob insert: %d %v", res.StatusCode, out)
	}
	if extra := stream.rest(1500 * time.Millisecond); len(extra) > 0 {
		t.Fatalf("alice saw bob's write: %v", extra)
	}
}
