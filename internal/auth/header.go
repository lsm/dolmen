package auth

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
)

const (
	PrincipalHeader = "X-Dolmen-Principal"
	GroupsHeader    = "X-Dolmen-Groups"

	HeaderSourceName = "trusted-proxy"

	DefaultMaxGroups = 128
	MinMaxGroups     = 1
	MaxMaxGroups     = 1024
)

var (
	principalRe = regexp.MustCompile(`^[!-~]{1,256}$`)
	groupRe     = regexp.MustCompile(`^[\x21-\x2B\x2D-\x7E]{1,128}$`)
)

func ValidateMaxGroups(n int) error {
	if n < MinMaxGroups || n > MaxMaxGroups {
		return fmt.Errorf("invalid max groups %d: must be between %d and %d", n, MinMaxGroups, MaxMaxGroups)
	}
	return nil
}

func ParseTrustedProxies(raw string) ([]*net.IPNet, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, cidr, err := net.ParseCIDR(entry); err == nil {
			out = append(out, cidr)
			continue
		}
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: use a CIDR range (10.0.0.0/8, 2001:db8::/32) or a bare IP address", entry)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		} else {
			ip = ip.To4()
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

type headerSource struct {
	trusted   []*net.IPNet
	maxGroups int
}

func NewHeaderSource(trusted []*net.IPNet, maxGroups int) Source {
	if maxGroups == 0 {
		maxGroups = DefaultMaxGroups
	}
	return &headerSource{trusted: trusted, maxGroups: maxGroups}
}

func (s *headerSource) Name() string { return HeaderSourceName }

func (s *headerSource) trusts(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	for _, cidr := range s.trusted {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *headerSource) Authenticate(r *http.Request) (Identity, bool) {
	if len(s.trusted) == 0 || !s.trusts(r) {
		return Identity{}, false
	}
	principal := r.Header.Get(PrincipalHeader)
	if principal == "" {
		return Identity{}, false
	}
	if !principalRe.MatchString(principal) || principal == AdminPrincipal {
		return Identity{}, false
	}
	groups, ok := parseGroups(r.Header.Get(GroupsHeader), s.maxGroups)
	if !ok {
		return Identity{}, false
	}
	return Identity{Principal: principal, Groups: groups, Source: s.Name()}, true
}

func parseGroups(raw string, maxGroups int) ([]string, bool) {
	if raw == "" {
		return nil, true
	}
	seen := make(map[string]struct{})
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !groupRe.MatchString(entry) {
			return nil, false
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
		if len(out) > maxGroups {
			return nil, false
		}
	}
	return out, true
}
