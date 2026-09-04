package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
)

// Auth-mode constants.
const (
	AuthModeOff = "off"
	AuthModeOn  = "on"

	// DefaultPrincipalHeader and DefaultGroupsHeader are the identity headers
	// forwarded by the trusted proxy. They are documented in the enterprise
	// design spec and may not be overridden unless the config explicitly asks.
	DefaultPrincipalHeader = "X-Dolmen-Principal"
	DefaultGroupsHeader    = "X-Dolmen-Groups"

	// BuiltinAdminPrincipal is the identity assumed when the bootstrap admin
	// key is presented. It is reserved: it may not be asserted via proxy headers.
	BuiltinAdminPrincipal = "dolmen-admin"

	maxPrincipalLen = 256
	maxGroupLen     = 128
	maxGroups       = 32
)

var (
	// principalRe matches one line of printable ASCII excluding space and
	// controls (0x21–0x7E). HTTP field parsing already rejects NUL/CR/LF and
	// most proxies strip leading/trailing spaces.
	principalRe = regexp.MustCompile(fmt.Sprintf(`^[!-~]{1,%d}$`, maxPrincipalLen))

	// groupRe matches a single group token: printable ASCII except comma.
	// Comma is the list separator, so it cannot appear inside a token.
	groupRe = regexp.MustCompile(fmt.Sprintf(`^[\x21-\x2b\x2d-\x7e]{1,%d}$`, maxGroupLen))

	// adminKeyRe matches RFC 6750 Bearer token68 grammar: base64url with no
	// padding, 32–256 characters.
	adminKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)
)

// Auth holds the parsed authentication configuration and performs per-request
// identity resolution. A nil or zero Auth is safe and means authentication is
// disabled.
type Auth struct {
	On              bool
	TrustedProxies  []*net.IPNet
	PrincipalHeader string
	GroupsHeader    string
	AdminKey        string
}

// Caller is the authenticated identity for one request.
type Caller struct {
	Principal string
	Groups    []string
}

type callerKey struct{}

// WithCaller returns a context carrying the resolved caller identity.
func WithCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the caller attached to ctx, if any.
func CallerFrom(ctx context.Context) *Caller {
	c, _ := ctx.Value(callerKey{}).(*Caller)
	return c
}

// NewAuth builds an Auth from configuration values. Mode must be "off" or "on";
// an empty value defaults to off. Header names default to the spec values; admin
// keys are validated whenever present. On auth=on, at least one of trusted
// proxies or admin key must be configured.
func NewAuth(mode, trustedProxies, principalHeader, groupsHeader, adminKey string) (*Auth, error) {
	if mode == "" {
		mode = AuthModeOff
	}
	if adminKey != "" && !adminKeyRe.MatchString(adminKey) {
		return nil, fmt.Errorf("DOLMEN_ADMIN_KEY must be base64url (token68, no padding) between 32 and 256 characters")
	}
	a := &Auth{
		PrincipalHeader: principalHeader,
		GroupsHeader:    groupsHeader,
		AdminKey:        adminKey,
	}
	switch mode {
	case AuthModeOff:
		return a, nil
	case AuthModeOn:
		// ok
	default:
		return nil, fmt.Errorf("DOLMEN_AUTH must be %q or %q, got %q", AuthModeOff, AuthModeOn, mode)
	}
	if a.PrincipalHeader == "" {
		a.PrincipalHeader = DefaultPrincipalHeader
	}
	if a.GroupsHeader == "" {
		a.GroupsHeader = DefaultGroupsHeader
	}
	cidrs, err := parseTrustedProxies(trustedProxies)
	if err != nil {
		return nil, fmt.Errorf("DOLMEN_TRUSTED_PROXIES: %w", err)
	}
	if len(cidrs) == 0 && a.AdminKey == "" {
		return nil, fmt.Errorf("DOLMEN_AUTH=on requires DOLMEN_TRUSTED_PROXIES or DOLMEN_ADMIN_KEY")
	}
	a.TrustedProxies = cidrs
	a.On = true
	return a, nil
}

// Authenticate resolves the caller for a request and returns a context carrying
// the identity. When auth is off it logs and ignores any credentials. When auth
// is on it returns an *Error with status 401 for missing or invalid identity.
func (a *Auth) Authenticate(ctx context.Context, r *http.Request) (context.Context, *Error) {
	if a == nil || !a.On {
		if a.credentialsPresent(r) {
			slog.Info("auth is off; ignoring credentials",
				slog.String("request_id", RequestIDFrom(ctx)),
				slog.String("remote_addr", r.RemoteAddr))
		}
		return ctx, nil
	}

	// Admin key is a direct credential and takes precedence over proxy headers.
	if a.AdminKey != "" {
		authz := r.Header.Get("Authorization")
		if authz != "" {
			if principal, ok := a.validateAdminKey(authz); ok {
				return WithCaller(ctx, &Caller{Principal: principal}), nil
			}
			return ctx, unauthorized("invalid admin key")
		}
	}

	// Without an admin key, identity must come from a trusted proxy.
	if len(a.TrustedProxies) == 0 {
		return ctx, unauthorized("no trusted proxies configured")
	}
	ip, err := remoteAddrIP(r)
	if err != nil {
		return ctx, unauthorized("cannot determine client address: %v", err)
	}
	trusted := false
	for _, cidr := range a.TrustedProxies {
		if cidr.Contains(ip) {
			trusted = true
			break
		}
	}
	if !trusted {
		return ctx, unauthorized("request not from a trusted proxy")
	}

	principal := strings.TrimSpace(r.Header.Get(a.PrincipalHeader))
	if principal == "" {
		return ctx, unauthorized("missing %s header", a.PrincipalHeader)
	}
	if !principalRe.MatchString(principal) {
		return ctx, unauthorized("malformed %s header", a.PrincipalHeader)
	}
	if principal == BuiltinAdminPrincipal {
		return ctx, unauthorized("%s header may not assert the reserved principal %q", a.PrincipalHeader, BuiltinAdminPrincipal)
	}

	groups, err := a.parseGroups(r.Header.Values(a.GroupsHeader))
	if err != nil {
		return ctx, unauthorized("malformed %s header: %v", a.GroupsHeader, err)
	}

	return WithCaller(ctx, &Caller{Principal: principal, Groups: groups}), nil
}

func (a *Auth) credentialsPresent(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	if a == nil {
		return false
	}
	hp, hg := a.PrincipalHeader, a.GroupsHeader
	if hp == "" {
		hp = DefaultPrincipalHeader
	}
	if hg == "" {
		hg = DefaultGroupsHeader
	}
	if r.Header.Get(hp) != "" {
		return true
	}
	if r.Header.Get(hg) != "" {
		return true
	}
	return false
}

func (a *Auth) validateAdminKey(authz string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		return "", false
	}
	token := strings.TrimSpace(authz[len(prefix):])
	if len(token) != len(a.AdminKey) {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(a.AdminKey)) != 1 {
		return "", false
	}
	return BuiltinAdminPrincipal, true
}

func (a *Auth) parseGroups(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	raw := strings.Join(values, ",")
	parts := strings.Split(raw, ",")
	var groups []string
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !groupRe.MatchString(p) {
			return nil, fmt.Errorf("group %q contains invalid characters", p)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		groups = append(groups, p)
	}
	if len(groups) > maxGroups {
		return nil, fmt.Errorf("too many groups (max %d)", maxGroups)
	}
	return groups, nil
}

func parseTrustedProxies(raw string) ([]*net.IPNet, error) {
	if raw == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			ip := net.ParseIP(p)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP %q", p)
			}
			if ip.To4() != nil {
				p = p + "/32"
			} else {
				p = p + "/128"
			}
		}
		_, cidr, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", p)
		}
		out = append(out, cidr)
	}
	return out, nil
}

func remoteAddrIP(r *http.Request) (net.IP, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// Some tests and a few transports do not include a port.
		if r.RemoteAddr == "" {
			return nil, fmt.Errorf("empty remote address")
		}
		ip := net.ParseIP(r.RemoteAddr)
		if ip == nil {
			return nil, fmt.Errorf("invalid remote address %q", r.RemoteAddr)
		}
		return ip, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("invalid remote address %q", r.RemoteAddr)
	}
	return ip, nil
}
