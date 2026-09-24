package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

func probe(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, body
}

func healthServer(t *testing.T, dir string) (*Server, *httptest.Server) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st, fakeEmb{})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func TestReadinessOnAHealthyServer(t *testing.T) {
	_, srv := healthServer(t, t.TempDir())
	for _, path := range []string{"/livez", "/healthz"} {
		if status, body := probe(t, srv.URL+path); status != 200 || body["status"] != "ok" {
			t.Fatalf("%s: %d %v", path, status, body)
		}
	}
	status, body := probe(t, srv.URL+"/readyz")
	if status != 200 || body["status"] != "ready" {
		t.Fatalf("readyz: %d %v", status, body)
	}
	if emb, _ := body["embedding"].(map[string]any); emb["status"] != "configured" {
		t.Fatalf("the embedding provider is reported without calling it: %v", body["embedding"])
	}
}

func TestReadinessFailsWhileDraining(t *testing.T) {
	s, srv := healthServer(t, t.TempDir())
	s.Drain()
	status, body := probe(t, srv.URL+"/readyz")
	if status != 503 || body["status"] != "not_ready" || !strings.Contains(strings.Join(anyStrings(body["reasons"]), " "), "draining") {
		t.Fatalf("readyz while draining: %d %v", status, body)
	}
	if status, _ := probe(t, srv.URL+"/livez"); status != 200 {
		t.Fatalf("a draining process is still alive, got %d", status)
	}
}

func TestReadinessFailsOnAnUnreadableNamespace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.db"), []byte("not a database, just bytes long enough to be read as a header ......."), 0o600); err != nil {
		t.Fatal(err)
	}
	_, srv := healthServer(t, dir)
	status, body := probe(t, srv.URL+"/readyz")
	if status != 503 || !strings.Contains(strings.Join(anyStrings(body["reasons"]), " "), "cannot be read") {
		t.Fatalf("readyz with a corrupt namespace: %d %v", status, body)
	}
	if strings.Contains(strings.Join(anyStrings(body["reasons"]), " "), dir) {
		t.Fatal("readiness must not reveal file paths")
	}
}

func TestReadinessFailsOnAnUnwritableDataDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through directory permissions")
	}
	dir := t.TempDir()
	_, srv := healthServer(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	status, body := probe(t, srv.URL+"/readyz")
	if status != 503 || !strings.Contains(strings.Join(anyStrings(body["reasons"]), " "), "not writable") {
		t.Fatalf("readyz with an unwritable data directory: %d %v", status, body)
	}
}

func TestDrainingClosesSubscriptionsWithAResumeCursor(t *testing.T) {
	s, srv := healthServer(t, t.TempDir())
	post(t, srv.URL, "create_namespace", map[string]any{"namespace": "feed"})
	res, err := http.Get(srv.URL + "/v1/subscribe?namespace=feed")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := bufio.NewScanner(res.Body)
	waitFor := func(prefix string) string {
		t.Helper()
		done := make(chan string, 1)
		go func() {
			for lines.Scan() {
				if strings.HasPrefix(lines.Text(), prefix) {
					done <- lines.Text()
					return
				}
			}
			done <- ""
		}()
		select {
		case l := <-done:
			if l == "" {
				t.Fatalf("stream ended before %q", prefix)
			}
			return l
		case <-time.After(10 * time.Second):
			t.Fatalf("no %q line", prefix)
		}
		return ""
	}
	waitFor("event: ready")
	s.Drain()
	waitFor("event: close")
	if l := waitFor("data:"); !strings.Contains(l, "cursor") {
		t.Fatalf("close frame without a cursor: %s", l)
	}
	if l := waitFor("data:"); !strings.Contains(l, "shutting down") {
		t.Fatalf("the error frame must say why: %s", l)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestReadinessRecoversOnceAnUnreadableNamespaceIsRemoved(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.db")
	if err := os.WriteFile(broken, []byte("not a database, just bytes long enough to be read as a header ......."), 0o600); err != nil {
		t.Fatal(err)
	}
	_, srv := healthServer(t, dir)
	if status, _ := probe(t, srv.URL+"/readyz"); status != 503 {
		t.Fatalf("readyz with a corrupt namespace: %d", status)
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	if status, body := probe(t, srv.URL+"/readyz"); status != 200 {
		t.Fatalf("readiness must recover without a restart once the file is gone: %d %v", status, body)
	}
}
