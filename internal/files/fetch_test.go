package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFetchHostRules(t *testing.T) {
	f := NewFetcher([]string{"files.example.com", "*.cdn.example.org"}, 0)
	for host, want := range map[string]bool{
		"files.example.com":      true,
		"FILES.example.com.":     true,
		"evil.files.example.com": false,
		"a.cdn.example.org":      true,
		"cdn.example.org":        false,
		"example.com":            false,
	} {
		if got := f.allowed(host); got != want {
			t.Errorf("allowed(%q) = %v, want %v", host, got, want)
		}
	}
	if !NewFetcher([]string{"*"}, 0).allowed("anything.test") {
		t.Fatal("* must allow any host")
	}
}

func TestNonPublicAddresses(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "fe80::1", "fc00::1", "0.0.0.0", "100.64.0.1", "224.0.0.1"} {
		if !nonPublic(net.ParseIP(ip)) {
			t.Errorf("%s must be refused", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2606:4700::1111"} {
		if nonPublic(net.ParseIP(ip)) {
			t.Errorf("%s is public", ip)
		}
	}
}

func testFetcher(srv *httptest.Server) *Fetcher {
	u, _ := url.Parse(srv.URL)
	f := NewFetcher([]string{u.Hostname()}, 0)
	f.blocked = func(net.IP) bool { return false }
	return f
}

func TestFetchStreamsAPublicFileIntoAPart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("a fetch must carry no credentials: %v", r.Header)
		}
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/reports/q3.pdf", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("%PDF fetched bytes"))
	}))
	defer srv.Close()
	s := New(Config{Root: t.TempDir()})
	r := s.NewRequest()
	defer r.Close()
	if err := r.ReceiveURL(context.Background(), "u", srv.URL+"/old", testFetcher(srv)); err != nil {
		t.Fatal(err)
	}
	b, err := r.Describe("u")
	if err != nil || b.Name != "q3.pdf" || b.MediaType != "application/pdf" || b.Size != 18 {
		t.Fatalf("fetched part = %+v %v", b, err)
	}
}

func TestFetchRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, "http://elsewhere.example/x", http.StatusFound)
		case "/broken":
			w.WriteHeader(http.StatusBadGateway)
		case "/big":
			w.Write([]byte(strings.Repeat("x", 100)))
		default:
			w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()
	s := New(Config{Root: t.TempDir(), MaxFileSize: 50})
	cases := []struct {
		label, url string
		f          *Fetcher
		want       string
	}{
		{"disabled", srv.URL, NewFetcher(nil, 0), "does not fetch URLs"},
		{"scheme", "file:///etc/passwd", testFetcher(srv), "not an http or https URL"},
		{"host outside the list", "http://other.example/x", testFetcher(srv), "not in -files-fetch-hosts"},
		{"loopback", srv.URL, func() *Fetcher { f := testFetcher(srv); f.blocked = nonPublic; return f }(), "not public"},
		{"redirect off the list", srv.URL + "/away", testFetcher(srv), "not in -files-fetch-hosts"},
		{"upstream error", srv.URL + "/broken", testFetcher(srv), "HTTP 502; the fetch may be retried"},
		{"too large", srv.URL + "/big", testFetcher(srv), "-max-file-size"},
	}
	for _, tc := range cases {
		r := s.NewRequest()
		err := r.ReceiveURL(context.Background(), "u", tc.url, tc.f)
		r.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error containing %q", tc.label, err, tc.want)
		}
	}
}

func TestFetchHostEntriesAreNormalized(t *testing.T) {
	f := NewFetcher([]string{" Files.Example.COM. ", "*.CDN.Example.org."}, 0)
	for _, host := range []string{"files.example.com", "a.cdn.example.org"} {
		if !f.allowed(host) {
			t.Errorf("%q must match its mixed-case, trailing-dot entry", host)
		}
	}
}

func TestFetchRefusesURLsCarryingCredentials(t *testing.T) {
	var sawAuth bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		if r.URL.Path == "/hop" {
			u, _ := url.Parse(srv.URL)
			http.Redirect(w, r, "http://user:pass@"+u.Host+"/x", http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	s := New(Config{Root: t.TempDir()})
	u, _ := url.Parse(srv.URL)
	for _, raw := range []string{"http://user:pass@" + u.Host + "/x", srv.URL + "/hop"} {
		r := s.NewRequest()
		err := r.ReceiveURL(context.Background(), "u", raw, testFetcher(srv))
		r.Close()
		if err == nil || !strings.Contains(err.Error(), "credentials") {
			t.Errorf("%s: %v, want a refusal naming the credentials", raw, err)
		}
	}
	if sawAuth {
		t.Fatal("credentials in a URL must never reach the host")
	}
}

func TestFetchesShareOneConnectionPool(t *testing.T) {
	var addrs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addrs = append(addrs, r.RemoteAddr)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	f := testFetcher(srv)
	s := New(Config{Root: t.TempDir()})
	for i := 0; i < 2; i++ {
		r := s.NewRequest()
		if err := r.ReceiveURL(context.Background(), "u", srv.URL, f); err != nil {
			t.Fatal(err)
		}
		r.Close()
	}
	if len(addrs) != 2 || addrs[0] != addrs[1] {
		t.Fatalf("a second fetch must reuse the first one's idle connection, not leak a new pool: %v", addrs)
	}
}

func TestEmbeddedAndReservedAddressesAreNotPublic(t *testing.T) {
	for _, ip := range []string{"64:ff9b::a00:1", "2002:a00:1::1", "2001:0:4136:e378::1", "240.0.0.1", "198.18.0.1", "192.0.0.1", "255.255.255.255"} {
		if !nonPublic(net.ParseIP(ip)) {
			t.Errorf("%s must be refused", ip)
		}
	}
}

func TestAFetchThatBreaksMidBodyIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		if r.URL.Path == "/stall" {
			time.Sleep(500 * time.Millisecond)
			return
		}
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()
	s := New(Config{Root: t.TempDir()})
	for path, want := range map[string]string{"/stall": "took longer than -files-fetch-timeout", "/cut": "broke off"} {
		f := testFetcher(srv)
		f.timeout = 200 * time.Millisecond
		r := s.NewRequest()
		err := r.ReceiveURL(context.Background(), "u", srv.URL+path, f)
		r.Close()
		var fe *Error
		if !errors.As(err, &fe) || fe.Status != http.StatusBadRequest || !strings.Contains(fe.Message, want) || !strings.Contains(fe.Message, "may be retried") {
			t.Errorf("%s: %v, want a classified 400 containing %q", path, err, want)
		}
	}
}

func TestFetchRefusalsNameTheCauseAndTheAlternative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := 0
		fmt.Sscanf(r.URL.Path, "/hop/%d", &n)
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n+1), http.StatusFound)
	}))
	defer srv.Close()
	s := New(Config{Root: t.TempDir()})
	for raw, want := range map[string][]string{
		srv.URL + "/hop/0":       {"more than 5 redirects", "multipart"},
		"example.com/report.pdf": {`"example.com/report.pdf"`, "multipart"},
	} {
		r := s.NewRequest()
		err := r.ReceiveURL(context.Background(), "u", raw, testFetcher(srv))
		r.Close()
		for _, w := range want {
			if err == nil || !strings.Contains(err.Error(), w) || strings.Contains(err.Error(), "may be retried") {
				t.Errorf("%s: %v, want %q and no retry advice", raw, err, w)
			}
		}
	}
}

func TestFiveRedirectsAreFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := 0
		fmt.Sscanf(r.URL.Path, "/hop/%d", &n)
		if n < 5 {
			http.Redirect(w, r, fmt.Sprintf("/hop/%d", n+1), http.StatusFound)
			return
		}
		w.Write([]byte("arrived"))
	}))
	defer srv.Close()
	s := New(Config{Root: t.TempDir()})
	r := s.NewRequest()
	defer r.Close()
	if err := r.ReceiveURL(context.Background(), "u", srv.URL+"/hop/0", testFetcher(srv)); err != nil {
		t.Fatalf("a chain of exactly 5 redirects must be followed: %v", err)
	}
}

func TestAFullDiskIsInsufficientStorage(t *testing.T) {
	err := storageErr(&fs.PathError{Op: "write", Path: "/data/.files-tmp/part-1", Err: syscall.ENOSPC})
	var fe *Error
	if !errors.As(err, &fe) || fe.Status != http.StatusInsufficientStorage || strings.Contains(fe.Message, "/data") {
		t.Fatalf("a full disk must be a 507 without the path, got %v", err)
	}
	other := &fs.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}
	if storageErr(other) != error(other) {
		t.Fatal("other storage errors pass through unchanged")
	}
}

func TestACanceledFetchIsNotATimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/body" {
			w.Header().Set("Content-Length", "100")
			w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	s := New(Config{Root: t.TempDir()})
	for _, path := range []string{"/head", "/body"} {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		r := s.NewRequest()
		err := r.ReceiveURL(ctx, "u", srv.URL+path, testFetcher(srv))
		r.Close()
		if !errors.Is(err, context.Canceled) || strings.Contains(fmt.Sprint(err), "took longer") {
			t.Errorf("%s: %v, want a cancellation, not a timeout", path, err)
		}
	}
}
