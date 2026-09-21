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

func changesAs(t *testing.T, h *harness, who, body string) (int, []any, string) {
	t.Helper()
	res, out := h.asIdentity(t, who, "", "changes_since", body)
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, nil, ""
	}
	data, _ := out["data"].(map[string]any)
	changes, _ := data["changes"].([]any)
	next, _ := data["next_cursor"].(string)
	return res.StatusCode, changes, next
}

func TestAScopedReaderCatchesUpOnItsOwnRowsOnly(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")

	for _, who := range []string{"bob", "alice", "bob", "alice", "bob"} {
		res, out := h.asIdentity(t, who, "", "insert",
			`{"namespace":"acme","table":"notes","records":[{"body":"from `+who+`"}]}`)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s insert: %d %v", who, res.StatusCode, out)
		}
	}

	status, changes, next := changesAs(t, h, "alice",
		`{"namespace":"acme","table":"notes","cursor":"begin"}`)
	if status != http.StatusOK {
		t.Fatalf("alice changes_since: status %d", status)
	}
	if len(changes) != 2 {
		t.Fatalf("alice wrote 2 of the 5 rows and must see exactly those: %v", changes)
	}
	for _, c := range changes {
		m, _ := c.(map[string]any)
		if _, leaked := m["owner"]; leaked {
			t.Fatalf("a change record exposed the owner label: %v", m)
		}
		if id, _ := m["row_id"].(float64); id != 2 && id != 4 {
			t.Fatalf("alice saw a row she does not own: %v", m)
		}
	}
	if next == "" {
		t.Fatal("no next cursor")
	}

	status, more, _ := changesAs(t, h, "alice", `{"namespace":"acme","table":"notes","cursor":"`+next+`"}`)
	if status != http.StatusOK {
		t.Fatalf("alice resume: status %d", status)
	}
	if len(more) != 0 {
		t.Fatalf("alice already caught up, so resuming must be quiet: %v", more)
	}
}

func TestAScopedReaderCannotTakeTheNamespaceWideFeed(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")

	res, out := h.asIdentity(t, "alice", "", "changes_since", `{"namespace":"acme","cursor":"begin"}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("the namespace feed reports every table, so a caller without namespace read must be refused: %d %v", res.StatusCode, out)
	}
}

func TestAScopedCursorLooksLikeAnyOther(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")

	for _, who := range []string{"alice", "bob", "bob", "bob", "alice"} {
		h.asIdentity(t, who, "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"from `+who+`"}]}`)
	}

	_, changes, _ := changesAs(t, h, "alice", `{"namespace":"acme","table":"notes","cursor":"begin"}`)
	if len(changes) != 2 {
		t.Fatalf("alice owns 2 rows: %v", changes)
	}
	seen := map[string]bool{}
	for _, c := range changes {
		m, _ := c.(map[string]any)
		tok, _ := m["cursor"].(string)
		if len(tok) != 32 {
			t.Fatalf("a cursor token is not the opaque fixed-width form, so its shape could carry a position: %q", tok)
		}
		if seen[tok] {
			t.Fatalf("two records share a cursor token: %q", tok)
		}
		seen[tok] = true
	}
}

func TestAScopedWaitIgnoresForeignTraffic(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")

	_, _, head := changesAs(t, h, "alice", `{"namespace":"acme","table":"notes","cursor":"begin"}`)

	res, out := h.asIdentity(t, "bob", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"from bob"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bob insert: %d %v", res.StatusCode, out)
	}
	res, out = h.asIdentity(t, "alice", "", "wait_for",
		`{"namespace":"acme","table":"notes","cursor":"`+head+`","timeout_ms":300}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice wait_for: %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if changes, _ := data["changes"].([]any); len(changes) != 0 {
		t.Fatalf("bob's write woke alice: %v", changes)
	}
	woken, _ := data["next_cursor"].(string)

	res, out = h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"from alice"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice insert: %d %v", res.StatusCode, out)
	}
	res, out = h.asIdentity(t, "alice", "", "wait_for",
		`{"namespace":"acme","table":"notes","cursor":"`+woken+`","timeout_ms":3000}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice second wait_for: %d %v", res.StatusCode, out)
	}
	data, _ = out["data"].(map[string]any)
	changes, _ := data["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("alice's own write must wake her: %v", changes)
	}
}

