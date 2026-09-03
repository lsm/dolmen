// Package skill bundles dolmen's layered skill markdown, renders it at serve
// time, and produces the JSON manifest agents use to pick the right layer.
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

// DefaultNamespaceHint is rendered when DOLMEN_SKILL_NAMESPACE_HINT is not set.
const DefaultNamespaceHint = "Everything lives in a namespace (an isolated database). Pick one namespace per project or user and stay in it. If this server is shared, the team that runs it will tell you which namespace to use; for a personal server, `default` is fine."

// Context is the data passed to every skill template at serve time.
type Context struct {
	BaseURL       string
	MCPURL        string
	Version       string
	NamespaceHint string
}

// ErrNotFound is returned when a skill name is not known.
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

// Manifest is the JSON discovery document at GET /skills.
type Manifest struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	BaseURL     string          `json:"base_url"`
	MCPURL      string          `json:"mcp_url"`
	Skills      []ManifestSkill `json:"skills"`
	LayerPicker string          `json:"layer_picker"`
}

// ManifestSkill describes one skill in the manifest.
type ManifestSkill struct {
	Name     string `json:"name"`
	Audience string `json:"audience"`
	Path     string `json:"path"`
	Layer    string `json:"layer"`
}

var (
	layerPickerTpl = template.Must(template.New("layer-picker").Parse(
		`Use the "dolmen" skill ({{.BaseURL}}/skills/dolmen) when you only query, insert, full-text/vector search, describe, list, or delete records in tables that already exist. Use the "dolmen-admin" skill ({{.BaseURL}}/skills/dolmen-admin) when you also design schemas, infer them from samples, create tables, migrate them, or perform other admin-only writes such as update, upsert, and upsert_by_key. Start every session by fetching the skill markdown, then connect to {{.MCPURL}}.`))

	mcpInstructionsTpl = template.Must(template.New("mcp-instructions").Parse(
		`Pick the right skill for this client from {{.BaseURL}}/skills, then connect to {{.MCPURL}} and begin by listing and describing tables. {{.NamespaceHint}}`))
)

// Render returns the rendered markdown for the named skill using ctx.
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

// ManifestJSON returns the JSON manifest for the current deployment.
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

// MCPInstructions returns the short usage summary for the MCP initialize result.
func MCPInstructions(ctx Context) string {
	s, err := renderString(mcpInstructionsTpl, ctx)
	if err != nil {
		// The static instructions template should never fail; fall back to a plain string.
		return fmt.Sprintf("Pick the right skill from %s/skills, then connect to %s.", ctx.BaseURL, ctx.MCPURL)
	}
	return s
}

// ETag returns a strong ETag for a named resource. It is derived from the
// version and the rendered body so that configuration changes (base URL,
// namespace hint) as well as version upgrades invalidate cached responses.
func ETag(name, version string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(version + "\n" + name + "\n"))
	_, _ = h.Write(body)
	return "\"" + hex.EncodeToString(h.Sum(nil)[:16]) + "\""
}

// BaseURLFor resolves the public base URL from the configured value, falling
// back to the request's scheme and host. It trims any trailing slash.
//
// Forwarded headers are parsed as comma-separated hop chains; the first
// (client-facing) value is used so the generated public URL reflects what the
// outermost trusted proxy saw, rather than a concatenation of every hop.
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
	return scheme + "://" + host
}

// forwardedFirst returns the first non-empty, trimmed value in a
// comma-separated header chain (e.g. X-Forwarded-Proto or X-Forwarded-Host).
func forwardedFirst(v string) string {
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			return p
		}
	}
	return v
}

// ContextFor builds a full render context from a request and configuration.
func ContextFor(r *http.Request, configuredBaseURL, namespaceHint, version string) Context {
	if namespaceHint == "" {
		namespaceHint = DefaultNamespaceHint
	}
	base := BaseURLFor(r, configuredBaseURL)
	return Context{
		BaseURL:       base,
		MCPURL:        base + "/mcp",
		Version:       version,
		NamespaceHint: namespaceHint,
	}
}

func renderString(t *template.Template, ctx Context) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", err
	}
	return buf.String(), nil
}
