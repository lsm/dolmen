package conformance

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func getURL(t *testing.T, link string) (int, string) {
	t.Helper()
	res, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestAFileURLDownloadsWithoutCredentialsUntilTheFileChanges(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "docs", "row_access": "own",
		"fields": []map[string]any{{"name": "title", "type": "string"}, {"name": "doc", "type": "file"}}})
	grantTo(t, h, "principal", "alice", "acme", "docs", "create", "update")
	grantTo(t, h, "principal", "bob", "acme", "docs", "create")
	if code, body := h.multipartAs(t, "alice", "insert", `{"namespace":"acme","table":"docs","records":[{"title":"a","doc":{"part":"f"}}]}`, map[string][]byte{"f": []byte("alice's file")}); code != http.StatusOK {
		t.Fatalf("alice insert: %d %v", code, body)
	}
	res, out := h.asIdentity(t, "alice", "", "file_url", `{"namespace":"acme","table":"docs","id":1,"field":"doc","ttl_seconds":60}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice file_url: %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	link, _ := data["url"].(string)
	if !strings.HasPrefix(link, h.srv.URL+"/v1/files?") {
		t.Fatalf("file_url must link to this server's download endpoint, got %q", link)
	}
	exp, err := time.Parse(time.RFC3339, data["expires_at"].(string))
	if err != nil || exp.Before(time.Now().Add(50*time.Second)) || exp.After(time.Now().Add(61*time.Second)) {
		t.Fatalf("expires_at %v must be ttl_seconds from now: %v", data["expires_at"], err)
	}
	if code, body := getURL(t, link); code != http.StatusOK || body != "alice's file" {
		t.Fatalf("a credential-less GET of the link: %d %q", code, body)
	}
	if code, _ := getURL(t, strings.Replace(link, "id=1", "id=2", 1)); code != http.StatusForbidden {
		t.Fatalf("a link edited to another row must be refused, got %d", code)
	}
	u, _ := url.Parse(link)
	q := u.Query()
	q.Set("sig", strings.Repeat("0", len(q.Get("sig"))))
	u.RawQuery = q.Encode()
	if code, _ := getURL(t, u.String()); code != http.StatusForbidden {
		t.Fatalf("a forged signature must be refused, got %d", code)
	}
	if res, out := h.asIdentity(t, "bob", "", "file_url", `{"namespace":"acme","table":"docs","id":1,"field":"doc"}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("bob asking for a link to alice's row must be refused as read_rows would hide it: %d %v", res.StatusCode, out)
	}
	if code, body := h.multipartAs(t, "alice", "update", `{"namespace":"acme","table":"docs","filter":"id = 1","set":{"doc":{"part":"f"}}}`, map[string][]byte{"f": []byte("a newer file")}); code != http.StatusOK {
		t.Fatalf("alice replacing her file: %d %v", code, body)
	}
	code, body := getURL(t, link)
	if code != http.StatusForbidden || !strings.Contains(body, "call file_url again") {
		t.Fatalf("a link to a replaced file must stop working: %d %s", code, body)
	}
}

func TestFileURLAgreesAcrossTransportsAndBoundsItsTTL(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"q3","doc":{"part":"notes"}}]}`, map[string][]byte{"notes": []byte("quarterly notes")}); code != http.StatusOK {
		t.Fatalf("multipart insert: %d %v", code, body)
	}
	input := map[string]any{"namespace": "fil", "table": "docs", "id": 1, "field": "doc"}
	viaMCP := h.mustMCP("file_url", input)
	viaHTTP := h.mustHTTP("file_url", input)
	for transport, out := range map[string]map[string]any{"mcp": viaMCP, "http": viaHTTP} {
		if len(out) != 2 || out["expires_at"] == nil {
			t.Fatalf("%s file_url must return exactly url and expires_at, got %v", transport, out)
		}
		link, _ := out["url"].(string)
		u, err := url.Parse(link)
		if err != nil || u.Path != "/v1/files" || u.Query().Get("sig") == "" {
			t.Fatalf("%s file_url link %q: %v", transport, link, err)
		}
		if code, body := getURL(t, link); code != http.StatusOK || body != "quarterly notes" {
			t.Fatalf("a link issued over %s: %d %q", transport, code, body)
		}
	}
	for _, ttl := range []int{0, 3601} {
		code, out := h.httpCall("file_url", map[string]any{"namespace": "fil", "table": "docs", "id": 1, "field": "doc", "ttl_seconds": ttl})
		if code != http.StatusBadRequest || !strings.Contains(errMessage(out), "ttl_seconds") {
			t.Fatalf("ttl_seconds %d: %d %v", ttl, code, out)
		}
	}
	code, out := h.httpCall("file_url", map[string]any{"namespace": "fil", "table": "docs", "id": 1, "field": "title"})
	if code != http.StatusBadRequest || !strings.Contains(errMessage(out), "not a file field") {
		t.Fatalf("file_url on a string field: %d %v", code, out)
	}
}
