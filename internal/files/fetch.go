package files

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/lsm/dolmen/internal/derr"
)

const DefaultFetchTimeout = 5 * time.Minute

type Fetcher struct {
	hosts   []string
	timeout time.Duration
	blocked func(net.IP) bool
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
	return &Fetcher{hosts: norm, timeout: timeout, blocked: nonPublic}
}

func (f *Fetcher) Enabled() bool { return f != nil && len(f.hosts) > 0 }

var sharedAddressSpace = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func nonPublic(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || sharedAddressSpace.Contains(ip)
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
		Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: nil, ForceAttemptHTTP2: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("more than 5 redirects")
			}
			return f.check(req.URL)
		},
	}
}

func (f *Fetcher) check(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fetchErr("%s is not an http or https URL", u.Scheme+":")
	}
	if u.User != nil {
		return fetchErr("URLs with credentials (user:password@) are not fetched, because the server sends no credentials; remove them, or download the file and send it as a multipart part")
	}
	if !f.allowed(u.Hostname()) {
		return fetchErr("host %q is not in -files-fetch-hosts; download the file and send it as a multipart part with {\"part\": \"<form name>\"} instead", u.Hostname())
	}
	return nil
}

func fetchErr(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: derr.InvalidRequest, Message: fmt.Sprintf(format, args...)}
}

func (r *Request) ReceiveURL(ctx context.Context, name, raw string, f *Fetcher) error {
	if !f.Enabled() {
		return fetchErr("this server does not fetch URLs (-files-fetch-hosts is empty); download the file with curl -o and send it as a multipart part with {\"part\": \"<form name>\"}")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fetchErr("%q is not a URL", raw)
	}
	if err := f.check(u); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fetchErr("%q is not a URL the server can fetch", raw)
	}
	res, err := f.client().Do(req)
	if err != nil {
		var fe *Error
		switch {
		case errors.As(err, &fe):
			return fe
		case errors.Is(err, errBlockedAddress):
			return fetchErr("fetching from %s was refused: it resolves to an address that is not public", u.Hostname())
		case ctx.Err() != nil:
			return fetchErr("fetching from %s took longer than -files-fetch-timeout (%s); the fetch may be retried", u.Hostname(), f.timeout)
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
	if filename == "/" || filename == "." {
		filename = ""
	}
	return r.Receive(name, filename, res.Header.Get("Content-Type"), res.Body)
}
