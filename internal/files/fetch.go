package files

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

const DefaultFetchTimeout = 5 * time.Minute

type FetchWording struct {
	Hosts    string
	Timeout  string
	Allow    string
	Fallback string
}

var ServerFetchWording = FetchWording{
	Hosts:    "-files-fetch-hosts",
	Timeout:  "-files-fetch-timeout",
	Allow:    "ask the operator to list the host in -files-fetch-hosts",
	Fallback: "over HTTP, download the file and send it as a multipart part with {\"part\": \"<form name>\"}; an MCP call cannot carry file bytes",
}

const FetchUserAgent = "dolmen (+https://github.com/lsm/dolmen)"

type Fetcher struct {
	say     FetchWording
	hosts   []string
	timeout time.Duration
	blocked func(net.IP) bool
	http    *http.Client
}

func NewFetcher(hosts []string, timeout time.Duration) *Fetcher {
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	norm := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")); h != "" {
			norm = append(norm, h)
		}
	}
	f := &Fetcher{say: ServerFetchWording, hosts: norm, timeout: timeout, blocked: nonPublic}
	f.http = f.client()
	return f
}

func (f *Fetcher) Worded(say FetchWording) *Fetcher {
	f.say = say
	return f
}

func (f *Fetcher) Enabled() bool { return f != nil && len(f.hosts) > 0 }

var reservedRanges = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{
		"100.64.0.0/10",
		"192.0.0.0/24",
		"198.18.0.0/15",
		"240.0.0.0/4",
		"64:ff9b::/96",
		"64:ff9b:1::/48",
		"2002::/16",
		"2001::/32",
	} {
		_, n, _ := net.ParseCIDR(cidr)
		out = append(out, n)
	}
	return out
}()

func nonPublic(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range reservedRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (f *Fetcher) allowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, h := range f.hosts {
		switch {
		case h == "*":
			return true
		case strings.HasPrefix(h, "*."):
			if strings.HasSuffix(host, h[1:]) {
				return true
			}
		case h == host:
			return true
		}
	}
	return false
}

var errBlockedAddress = errors.New("the address is not public")

func (f *Fetcher) client() *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || f.blocked(ip) {
			return errBlockedAddress
		}
		return nil
	}}
	return &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: nil, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 4},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return fetchErr("fetching from %s followed more than 5 redirects; give the file's final URL, or %s", via[0].URL.Hostname(), f.say.Fallback)
			}
			return f.check(req.URL)
		},
	}
}

func (f *Fetcher) check(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fetchErr("%q is not an http or https URL; give the file's full http(s) URL, or %s", u.String(), f.say.Fallback)
	}
	if u.User != nil {
		return fetchErr("URLs with credentials (user:password@) are not fetched, because no credentials are sent; remove them, or %s", f.say.Fallback)
	}
	if !f.allowed(u.Hostname()) {
		return fetchErr("host %q is not in %s; %s, or %s", u.Hostname(), f.say.Hosts, f.say.Allow, f.say.Fallback)
	}
	return nil
}

func fetchErr(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: derr.InvalidRequest, Message: fmt.Sprintf(format, args...)}
}

func (r *Request) ReceiveURL(ctx context.Context, name, raw string, f *Fetcher) error {
	if !f.Enabled() {
		return fetchErr("this server does not fetch URLs (%s is empty); %s, or %s", f.say.Hosts, f.say.Allow, f.say.Fallback)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fetchErr("%q is not a URL", raw)
	}
	if err := f.check(u); err != nil {
		return err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fetchErr("%q is not a URL the server can fetch", raw)
	}
	req.Header.Set("User-Agent", FetchUserAgent)
	res, err := f.http.Do(req)
	if err != nil {
		var fe *Error
		switch {
		case errors.As(err, &fe):
			return fe
		case errors.Is(err, errBlockedAddress):
			return fetchErr("fetching from %s was refused: it resolves to an address that is not public", u.Hostname())
		case parent.Err() != nil:
			return parent.Err()
		case ctx.Err() != nil:
			return fetchErr("fetching from %s took longer than %s (%s); the fetch may be retried", u.Hostname(), f.say.Timeout, f.timeout)
		}
		return fetchErr("fetching from %s failed before a response arrived; the fetch may be retried", u.Hostname())
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		retry := ""
		if res.StatusCode >= 500 {
			retry = "; the fetch may be retried"
		}
		return fetchErr("fetching from %s answered HTTP %d%s", u.Hostname(), res.StatusCode, retry)
	}
	filename := path.Base(res.Request.URL.Path)
	if _, params, err := mime.ParseMediaType(res.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		filename = params["filename"]
	}
	if filename = BaseName(filename); schema.FileNameRule(filename) != "" {
		filename = ""
	}
	err = r.Receive(name, filename, res.Header.Get("Content-Type"), res.Body)
	var fe *Error
	var pe *fs.PathError
	switch {
	case err == nil || errors.As(err, &fe) || errors.As(err, &pe):
		return err
	case parent.Err() != nil:
		return parent.Err()
	case ctx.Err() != nil:
		return fetchErr("fetching from %s took longer than %s (%s); the fetch may be retried", u.Hostname(), f.say.Timeout, f.timeout)
	}
	return fetchErr("fetching from %s broke off before the file arrived; the fetch may be retried", u.Hostname())
}

func (s *Store) UploadTimeout() time.Duration {
	if !s.Remote() {
		return 0
	}
	return s.fetcher.timeout
}

func (s *Store) FetchTimeout() time.Duration {
	if !s.fetcher.Enabled() {
		return 0
	}
	return s.fetcher.timeout
}

func (r *Request) Fetch(ctx context.Context, raw string) (string, error) {
	if name, ok := r.fetched[raw]; ok {
		return name, nil
	}
	if r.fetched == nil {
		r.fetched = map[string]string{}
	}
	var salt [8]byte
	rand.Read(salt[:])
	name := fmt.Sprintf("url-%x-%d", salt, len(r.fetched)+1)
	if err := r.ReceiveURL(ctx, name, raw, r.s.fetcher); err != nil {
		return "", err
	}
	r.fetched[raw] = name
	return name, nil
}
