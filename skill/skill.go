package skill

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

func BaseURLFor(r *http.Request, configured string) string {
	if configured != "" {
		return strings.TrimRight(configured, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = forwardedFirst(p)
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = forwardedFirst(h)
	}
	prefix := ""
	if p := r.Header.Get("X-Forwarded-Prefix"); p != "" {
		prefix = NormalizePrefix(forwardedFirst(p))
	}
	return scheme + "://" + host + prefix
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

func NormalizePrefix(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimRight(v, "/")
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
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
