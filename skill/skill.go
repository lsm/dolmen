package skill

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/template"
)

//go:embed dolmen.md
var dolmenRaw []byte

//go:embed dolmen-admin.md
var adminRaw []byte

const DefaultNamespaceHint = "Everything lives in a namespace (an isolated database). Pick one namespace per project or user and stay in it. If this server is shared, the team that runs it will tell you which namespace to use; for a personal server, `default` is fine."

type Context struct {
	BaseURL       string
	MCPURL        string
	Version       string
	NamespaceHint string
}

var ErrNotFound = errors.New("unknown skill")

type skill struct {
	Name     string
	Layer    string
	Audience string
	Path     string
	tmpl     *template.Template
}

var (
	dolmen      *skill
	dolmenAdmin *skill
	byName      map[string]*skill
)

func init() {
	dolmen = &skill{
		Name:     "dolmen",
		Layer:    "core",
		Audience: "end-user agents and assistants",
		Path:     "/skills/dolmen",
		tmpl:     template.Must(template.New("dolmen").Parse(string(dolmenRaw))),
	}
	dolmenAdmin = &skill{
		Name:     "dolmen-admin",
		Layer:    "admin",
		Audience: "developer and infrastructure agents that design schemas and run migrations",
		Path:     "/skills/dolmen-admin",
		tmpl:     template.Must(template.New("dolmen-admin").Parse(string(adminRaw))),
	}
	byName = map[string]*skill{
		dolmen.Name:      dolmen,
		dolmenAdmin.Name: dolmenAdmin,
	}
}

type Manifest struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	BaseURL     string          `json:"base_url"`
	MCPURL      string          `json:"mcp_url"`
	OpenAPIURL  string          `json:"openapi_url"`
	Skills      []ManifestSkill `json:"skills"`
	LayerPicker string          `json:"layer_picker"`
}

type ManifestSkill struct {
	Name     string `json:"name"`
	Audience string `json:"audience"`
	Path     string `json:"path"`
	Layer    string `json:"layer"`
}

var (
	layerPickerTpl = template.Must(template.New("layer-picker").Parse(
		`Use the "dolmen" skill ({{.BaseURL}}/skills/dolmen) when you only query, insert, full-text/vector search, describe, list, or delete records in tables that already exist. Use the "dolmen-admin" skill ({{.BaseURL}}/skills/dolmen-admin) when you also design schemas, infer them from samples, create tables, migrate them, or perform other admin-only writes such as update, upsert, and upsert_by_key. Start every session by fetching the skill markdown, then connect to {{.MCPURL}}; if the MCP tools cannot be hot-loaded into the running session, both skills document a stateless JSON-RPC fallback that drives the same endpoint directly.`))

	mcpInstructionsTpl = template.Must(template.New("mcp-instructions").Parse(
		`Pick the right skill for this client from {{.BaseURL}}/skills, then connect to {{.MCPURL}} and begin by listing and describing tables. {{.NamespaceHint}}`))

	stdioInstructionsTpl = template.Must(template.New("stdio-instructions").Parse(
		`This dolmen MCP server runs over stdio: begin by listing and describing tables; tools/list is the authoritative surface.{{if .BaseURL}} The HTTP deployment — skills and the REST API — is at {{.BaseURL}}.{{end}} {{.NamespaceHint}}`))
)

func Render(name string, ctx Context) ([]byte, error) {
	s, ok := byName[name]
	if !ok {
		return nil, ErrNotFound
	}
	var buf bytes.Buffer
	if err := s.tmpl.Execute(&buf, ctx); err != nil {
		return nil, fmt.Errorf("render skill %q: %w", name, err)
	}
	return buf.Bytes(), nil
}

func ManifestJSON(ctx Context) ([]byte, error) {
	picker, err := renderString(layerPickerTpl, ctx)
	if err != nil {
		return nil, fmt.Errorf("render layer picker: %w", err)
	}
	m := Manifest{
		Name:        "dolmen",
		Version:     ctx.Version,
		BaseURL:     ctx.BaseURL,
		MCPURL:      ctx.MCPURL,
		OpenAPIURL:  ctx.BaseURL + "/v1/openapi.json",
		LayerPicker: picker,
	}
	for _, s := range []*skill{dolmen, dolmenAdmin} {
		m.Skills = append(m.Skills, ManifestSkill{
			Name:     s.Name,
			Audience: s.Audience,
			Path:     s.Path,
			Layer:    s.Layer,
		})
	}
	return json.MarshalIndent(m, "", "  ")
}

func MCPInstructions(ctx Context) string {
	s, err := renderString(mcpInstructionsTpl, ctx)
	if err != nil {

		return fmt.Sprintf("Pick the right skill from %s/skills, then connect to %s.", ctx.BaseURL, ctx.MCPURL)
	}
	return s
}

func StdioInstructions(ctx Context) string {
	s, err := renderString(stdioInstructionsTpl, ctx)
	if err != nil {

		return fmt.Sprintf("This dolmen MCP server runs over stdio: begin by listing and describing tables; tools/list is the authoritative surface. %s", ctx.NamespaceHint)
	}
	return s
}

func ETag(name, version string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(version + "\n" + name + "\n"))
	_, _ = h.Write(body)
	return "\"" + hex.EncodeToString(h.Sum(nil)[:16]) + "\""
}

func UsableRequestHost(r *http.Request, configured string) bool {
	if configured != "" {
		return true
	}
	if h := forwardedFirst(r.Header.Get("X-Forwarded-Host")); h != "" && validHost(h) {
		return true
	}
	if fwd := parseForwarded(r.Header.Get("Forwarded")); r.Header.Get("X-Forwarded-Host") == "" && fwd.host != "" && validHost(fwd.host) {
		return true
	}
	return validHost(r.Host)
}

func BaseURLFor(r *http.Request, configured string) string {
	if configured != "" {
		return strings.TrimRight(configured, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := forwardedFirst(r.Header.Get("X-Forwarded-Proto")); validScheme(p) {
		scheme = p
	}
	host := r.Host
	if !validHost(host) {
		host = "invalid-host.invalid"
	}
	if h := forwardedFirst(r.Header.Get("X-Forwarded-Host")); h != "" && validHost(h) {
		host = h
	}
	if fwd := parseForwarded(r.Header.Get("Forwarded")); fwd.host != "" || fwd.proto != "" {
		if r.Header.Get("X-Forwarded-Proto") == "" && validScheme(fwd.proto) {
			scheme = fwd.proto
		}
		if r.Header.Get("X-Forwarded-Host") == "" && fwd.host != "" && validHost(fwd.host) {
			host = fwd.host
		}
	}
	prefix := ""
	if p := r.Header.Get("X-Forwarded-Prefix"); p != "" {
		prefix = NormalizePrefix(forwardedFirst(p))
	} else {
		prefix = StrippedPrefix(r)
	}
	return scheme + "://" + host + prefix
}

var PublicURLVaryHeader = strings.Join(append([]string{
	"Host", "Forwarded", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Prefix",
}, originalURIHeaders...), ", ")

var ForwardingHeaders = append([]string{
	"Forwarded", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Prefix",
}, originalURIHeaders...)

var originalURIHeaders = []string{
	"X-Forwarded-Uri",
	"X-Original-Uri",
	"X-Original-Url",
	"X-Envoy-Original-Path",
	"X-Rewrite-Url",
}

func StrippedPrefix(r *http.Request) string {
	current := r.URL.EscapedPath()
	if current == "" {
		current = "/"
	}
	for _, name := range originalURIHeaders {
		raw := forwardedFirst(r.Header.Get(name))
		if raw == "" {
			continue
		}
		original := originalPath(raw)
		if original == "" || original == current {
			continue
		}
		if !strings.HasSuffix(original, current) {
			continue
		}
		if p := NormalizePrefix(strings.TrimSuffix(original, current)); p != "" {
			return p
		}
	}
	return ""
}

func originalPath(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		raw = u.EscapedPath()
	}
	if raw != "" && !strings.HasPrefix(raw, "/") {
		return ""
	}
	return raw
}

type forwardedPair struct {
	proto string
	host  string
}

func parseForwarded(v string) forwardedPair {
	var out forwardedPair
	first := forwardedFirst(v)
	if first == "" {
		return out
	}
	for _, part := range strings.Split(first, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if value == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "proto":
			out.proto = value
		case "host":
			out.host = value
		}
	}
	return out
}

func Proxied(r *http.Request) bool {
	if r.Header.Get("Forwarded") != "" {
		return true
	}
	for _, name := range append([]string{"X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Prefix", "X-Forwarded-For"}, originalURIHeaders...) {
		if r.Header.Get(name) != "" {
			return true
		}
	}
	return false
}

func UnreachableBaseURL(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

const ProxyAdvice = "the public links dolmen advertises (the skills manifest, the skill markdown, openapi.json servers, and the MCP initialize instructions) are built from this request, and it arrived through a proxy that did not say what the public URL is; set DOLMEN_BASE_URL to the full public URL, or list the proxy in DOLMEN_TRUSTED_PROXIES and have it send Host/X-Forwarded-Host, X-Forwarded-Proto, and X-Forwarded-Prefix (forwarding headers from unlisted peers are dropped; nginx defaults Host to the upstream address and never sends X-Forwarded-Prefix on its own)"

var hostLabelRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

var ipv6LiteralRe = regexp.MustCompile(`^\[[0-9A-Fa-f:.]{2,45}\]$`)

func validScheme(s string) bool {
	return s == "http" || s == "https"
}

func validPort(s string) bool {
	if len(s) < 2 || len(s) > 6 || s[0] != ':' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func validHost(h string) bool {
	if h == "" || len(h) > 260 {
		return false
	}
	if strings.HasPrefix(h, "[") {
		end := strings.LastIndex(h, "]")
		if end < 0 || !ipv6LiteralRe.MatchString(h[:end+1]) {
			return false
		}
		rest := h[end+1:]
		return rest == "" || validPort(rest)
	}
	if i := strings.LastIndex(h, ":"); i >= 0 {
		if !validPort(h[i:]) {
			return false
		}
		h = h[:i]
	}
	return hostLabelRe.MatchString(h)
}

func forwardedFirst(v string) string {
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			return p
		}
	}
	return v
}

func ContextFor(r *http.Request, configuredBaseURL, namespaceHint, version, prefix string) Context {
	if namespaceHint == "" {
		namespaceHint = DefaultNamespaceHint
	}
	base := publicBase(BaseURLFor(r, configuredBaseURL), prefix)
	return Context{
		BaseURL:       base,
		MCPURL:        base + "/mcp",
		Version:       version,
		NamespaceHint: namespaceHint,
	}
}

func publicBase(base, prefix string) string {
	prefix = NormalizePrefix(prefix)
	if prefix == "" || strings.HasSuffix(base, prefix) {
		return base
	}
	return base + prefix
}

const (
	MaxPrefixBytes    = 128
	MaxPrefixSegments = 8
)

var prefixSegmentRe = regexp.MustCompile(`^[A-Za-z0-9._~:@-]+$`)

func NormalizePrefix(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimRight(v, "/")
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
	}
	if len(v) > MaxPrefixBytes {
		return ""
	}
	segments := strings.Split(strings.TrimPrefix(v, "/"), "/")
	if len(segments) > MaxPrefixSegments {
		return ""
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." || !prefixSegmentRe.MatchString(seg) {
			return ""
		}
	}
	return v
}

func renderString(t *template.Template, ctx Context) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", err
	}
	return buf.String(), nil
}
