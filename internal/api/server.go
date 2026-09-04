package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/version"
	"github.com/lsm/dolmen/skill"
)

type Server struct {
	st            *store.Store
	emb           embed.Provider
	baseURL       string
	namespaceHint string
	prefix        string
}

// Option customizes a Server.
type Option func(*Server)

// WithBaseURL sets a configured public base URL. If empty, the request Host is used.
func WithBaseURL(u string) Option {
	return func(s *Server) {
		s.baseURL = strings.TrimRight(u, "/")
	}
}

// WithPrefix sets the server prefix to include in rendered skill and MCP links.
func WithPrefix(p string) Option {
	return func(s *Server) {
		s.prefix = skill.NormalizePrefix(p)
	}
}

// WithNamespaceHint sets the namespace guidance rendered into skills.
func WithNamespaceHint(h string) Option {
	return func(s *Server) {
		s.namespaceHint = h
	}
}

func New(st *store.Store, emb embed.Provider, opts ...Option) *Server {
	s := &Server{st: st, emb: emb}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type OpDef struct {
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Func         func(ctx context.Context, s *Server, body []byte) (any, error)
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func fieldNameProp(desc string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": desc,
		"pattern":     schema.IdentPattern(),
		"not": map[string]any{
			"enum": schema.ReservedFieldNames(),
		},
	}
}

// existingFieldNameProp matches any syntactically valid field name, including
// legacy keyword or reserved names that existed before the stricter rules. It
// is used for migration references (from, name) so clients can rename or drop
// fields created under the old validation.
func existingFieldNameProp(desc string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": desc,
		"pattern":     `^[a-z][a-z0-9_]{0,63}$`,
	}
}

// fieldItemSchema describes a field definition. withDefault additionally
// declares the create_table-only default annotation: migrate's add_field takes
// its backfill default on the change itself, so its field object must not
// accept one.
func fieldItemSchema(desc string, withDefault bool) map[string]any {
	properties := map[string]any{
		"name": map[string]any{
			"type":        "string",
			"description": "Field name (lowercase [a-z0-9_], max 64 chars; not a SQLite/SQL keyword or reserved name)",
			"pattern":     schema.IdentPattern(),
			"not": map[string]any{
				"enum": schema.ReservedFieldNames(),
			},
		},
		"type": map[string]any{
			"type":        "string",
			"description": "One of: string, text, number, boolean, timestamp, json, vector (omit to default to string)",
			"enum": []schema.FieldType{
				schema.String, schema.Text, schema.Number, schema.Boolean,
				schema.Timestamp, schema.JSON, schema.Vector,
			},
		},
		"fulltext":  prop("boolean", "Index this field for full-text search (string/text only)"),
		"vectorize": prop("boolean", "Server embeds this text field automatically (string/text only, one per table)"),
		"dim": map[string]any{
			"type":        "integer",
			"description": "Dimension for vector fields",
			"minimum":     1,
			"maximum":     schema.MaxVectorDim,
		},
		"required": prop("boolean", "Reject inserts that omit this field"),
		"enum": map[string]any{
			"type":        "array",
			"description": "Closed vocabulary for a string field: writes carrying any other value are rejected. Exact match, no case folding — values are stored as written. The field's default (when set) must be one of these values; change the vocabulary later with migrate set_enum",
			"items":       map[string]any{"type": "string", "minLength": 1},
			"minItems":    1,
			"uniqueItems": true,
		},
	}
	allOf := []any{
		map[string]any{
			"if": map[string]any{
				"properties": map[string]any{"type": map[string]any{"const": string(schema.Vector)}},
				"required":   []string{"type"},
			},
			"then": map[string]any{"required": []string{"dim"}},
			"else": map[string]any{"not": map[string]any{"required": []string{"dim"}}},
		},
		map[string]any{
			"if": map[string]any{
				"properties": map[string]any{"fulltext": map[string]any{"const": true}},
				"required":   []string{"fulltext"},
			},
			"then": map[string]any{
				"properties": map[string]any{
					"type": map[string]any{"enum": []schema.FieldType{schema.String, schema.Text}},
					"name": map[string]any{"not": map[string]any{"const": "rank"}},
				},
			},
		},
		map[string]any{
			"if": map[string]any{
				"properties": map[string]any{"vectorize": map[string]any{"const": true}},
				"required":   []string{"vectorize"},
			},
			"then": map[string]any{
				"properties": map[string]any{
					"type": map[string]any{"enum": []schema.FieldType{schema.String, schema.Text}},
				},
			},
		},
		map[string]any{
			// enum is string-only; an omitted type defaults to string, so the
			// guard fires only when a type is present and is not string.
			"if": map[string]any{
				"properties": map[string]any{"type": map[string]any{"not": map[string]any{"const": string(schema.String)}}},
				"required":   []string{"type"},
			},
			"then": map[string]any{"not": map[string]any{"required": []string{"enum"}}},
		},
	}
	if withDefault {
		properties["default"] = map[string]any{
			"description": "Value stored when an insert omits the field — string/text: a string; timestamp: an ISO/RFC3339 string; number: a number; boolean: a boolean; json: any JSON value; vector: a number array of dim entries. Must match the field's type; not allowed on required or vectorize fields",
		}
		allOf = append(allOf,
			map[string]any{
				"if": map[string]any{
					"properties": map[string]any{"required": map[string]any{"const": true}},
					"required":   []string{"required"},
				},
				"then": map[string]any{"not": map[string]any{"required": []string{"default"}}},
			},
			map[string]any{
				"if": map[string]any{
					"properties": map[string]any{"vectorize": map[string]any{"const": true}},
					"required":   []string{"vectorize"},
				},
				"then": map[string]any{"not": map[string]any{"required": []string{"default"}}},
			},
		)
	}
	return map[string]any{
		"description":          desc,
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
		"required":             []string{"name"},
		"allOf":                allOf,
	}
}

func nsProp(desc string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": desc,
		"pattern":     `^[a-z0-9][a-z0-9_-]{0,63}$`,
	}
}

func tableProp(desc string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": desc,
		"pattern":     schema.IdentPattern(),
		"not": map[string]any{
			"anyOf": []any{
				map[string]any{"pattern": "__fts"},
				map[string]any{"pattern": "^sqlite_"},
				map[string]any{"pattern": "^pragma_"},
				map[string]any{"enum": schema.ReservedTableNames()},
			},
		},
	}
}

// existingTableProp matches any syntactically valid table name, including
// legacy keyword or reserved names that were accepted by earlier releases. It
// keeps the __fts and sqlite_ guards because those are never valid user tables.
func existingTableProp(desc string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": desc,
		"pattern":     `^[a-z][a-z0-9_]{0,63}$`,
		"not": map[string]any{
			"anyOf": []any{
				map[string]any{"pattern": "__fts"},
				map[string]any{"pattern": "^sqlite_"},
			},
		},
	}
}

func (s *Server) embedder() store.Embedder {
	return store.Embedder{
		Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
			return s.emb.Embed(ctx, texts)
		},
		Identity: s.emb.Identity(),
	}
}

type nsReq struct {
	Namespace string `json:"namespace"`
}

type tableReq struct {
	Namespace string `json:"namespace"`
	Table     string `json:"table"`
}

type createTableReq struct {
	Namespace string         `json:"namespace"`
	Table     string         `json:"table"`
	Fields    []schema.Field `json:"fields"`
}

type inferReq struct {
	Samples []map[string]any `json:"samples"`
}

func decode(body []byte, v any) error {
	if len(body) == 0 {
		return badRequest("empty request body")
	}
	var probe any
	probeDec := json.NewDecoder(bytes.NewReader(body))
	probeDec.UseNumber()
	if err := probeDec.Decode(&probe); err != nil {
		return badRequest("invalid JSON: %v", err)
	}
	if err := rejectNulls("", probe); err != nil {
		return err
	}
	return decodeData(body, v)
}

func decodeData(body []byte, v any) error {
	if len(body) == 0 {
		return badRequest("empty request body")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return badRequest("unexpected trailing content after JSON body")
	}
	return nil
}

// decodeAllowNullArgs is like decode but permits null values inside the
// "args" array so SQL-filter bind parameters can include NULL.
func decodeAllowNullArgs(body []byte, v any) error {
	if len(body) == 0 {
		return badRequest("empty request body")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var probe map[string]any
	if err := dec.Decode(&probe); err != nil {
		return badRequest("invalid JSON: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return badRequest("unexpected trailing content after JSON body")
	}
	for k, val := range probe {
		if k == "args" {
			continue
		}
		if err := rejectNulls(k, val); err != nil {
			return err
		}
	}
	return decodeData(body, v)
}

// jsonDefaultPathRe matches paths inside a default value — a migrate change's
// (changes[0].default.…) or a create_table field's (fields[0].default.…) —
// whether object-shaped or array-shaped (fields[0].default[…]). Nested nulls
// there are JSON data the store coerces and serializes as-is; everywhere else
// null remains a request error.
var jsonDefaultPathRe = regexp.MustCompile(`^(?:changes|fields)\[\d+\]\.default(?:\.|\[)`)

func rejectNulls(path string, v any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if val == nil {
				if jsonDefaultPathRe.MatchString(p) {
					continue
				}
				return badRequest("null is not allowed for %q", p)
			}
			if err := rejectNulls(p, val); err != nil {
				return err
			}
		}
	case []any:
		for i, val := range t {
			p := fmt.Sprintf("%s[%d]", path, i)
			if val == nil {
				if jsonDefaultPathRe.MatchString(p) {
					continue
				}
				return badRequest("null is not allowed for %q", p)
			}
			if err := rejectNulls(p, val); err != nil {
				return err
			}
		}
	}
	return nil
}

func normNS(ns string) string {
	return strings.ToLower(strings.TrimSpace(ns))
}

func normTable(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}

func limit(n int) int {
	if n <= 0 {
		return 10
	}
	if n > 200 {
		return 200
	}
	return n
}

func OpNames() []string {
	names := make([]string, 0, len(Ops))
	for name := range Ops {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Server) Dispatch(ctx context.Context, op string, body []byte) (any, error) {
	def, ok := Ops[op]
	if !ok {
		return nil, notFound("unknown operation %q", op)
	}
	return def.Func(ctx, s, body)
}

func OriginGuard(next http.Handler, extraOrigins []string) http.Handler {
	localHosts := map[string]bool{"localhost": true, "127.0.0.1": true, "[::1]": true, "::1": true}
	exact := map[string]bool{}
	for _, o := range extraOrigins {
		exact[strings.ToLower(strings.TrimRight(o, "/"))] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			allowed := exact[strings.ToLower(strings.TrimRight(origin, "/"))]
			if !allowed {
				if u, err := url.Parse(origin); err == nil && localHosts[strings.ToLower(u.Hostname())] {
					allowed = true
				}
			}
			if !allowed {
				writeError(w, r, forbidden("origin not allowed"))
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version, MCP-Session-Id, X-Request-Id")
				w.Header().Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if r.Method == http.MethodPost {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || strings.ToLower(mt) != "application/json" {
				writeError(w, r, &Error{Status: http.StatusUnsupportedMediaType, Code: ErrCodeInvalid, Message: "content-type must be application/json"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"name": "dolmen", "version": version.Version})
	})
	mux.HandleFunc("/skills", s.handleSkillsManifest)
	mux.HandleFunc("/skills/", s.handleSkill)
	mux.HandleFunc("/v1/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		// Assign the request id before any error path so every response,
		// envelope, and log line carries one — echoed when the client sent
		// X-Request-Id, generated when it did not.
		r = r.WithContext(WithRequestID(r.Context(), RequestIDFor(r)))
		w.Header().Set("X-Request-Id", RequestIDFrom(r.Context()))
		op := strings.TrimPrefix(r.URL.Path, "/v1/")
		if op == "" || strings.Contains(op, "/") {
			writeError(w, r, notFound("unknown operation"))
			return
		}
		if _, ok := Ops[op]; !ok {
			writeError(w, r, notFound("unknown operation"))
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, r, &Error{Status: http.StatusMethodNotAllowed, Code: ErrCodeInvalid, Message: "use POST"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, r, &Error{Status: http.StatusRequestEntityTooLarge, Code: ErrCodeInvalid, Message: "request body exceeds the 32 MiB limit"})
				return
			}
			writeError(w, r, badRequest("cannot read body"))
			return
		}
		res, err := s.Dispatch(r.Context(), op, body)
		if err != nil {
			slog.Debug("op failed", "op", op, "err", err)
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": res})
	})
	return mux
}

func (s *Server) handleSkillsManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, r, &Error{Status: http.StatusMethodNotAllowed, Code: ErrCodeInvalid, Message: "use GET"})
		return
	}
	ctx := skill.ContextFor(r, s.baseURL, s.namespaceHint, version.Version, s.prefix)
	manifest, err := skill.ManifestJSON(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.serveSkillBytes(w, r, manifest, "manifest", "application/json")
}

func (s *Server) handleSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, r, &Error{Status: http.StatusMethodNotAllowed, Code: ErrCodeInvalid, Message: "use GET"})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/skills/")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, r, notFound("unknown skill %q", name))
		return
	}
	ctx := skill.ContextFor(r, s.baseURL, s.namespaceHint, version.Version, s.prefix)
	body, err := skill.Render(name, ctx)
	if err != nil {
		if errors.Is(err, skill.ErrNotFound) {
			writeError(w, r, notFound("unknown skill %q", name))
		} else {
			writeError(w, r, err)
		}
		return
	}
	s.serveSkillBytes(w, r, body, name, "text/markdown; charset=utf-8")
}

func (s *Server) serveSkillBytes(w http.ResponseWriter, r *http.Request, body []byte, name, contentType string) {
	etag := skill.ETag(name, version.Version, body)
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", contentType)
	if etagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func etagMatch(r *http.Request, etag string) bool {
	in := r.Header.Get("If-None-Match")
	if in == "" {
		return false
	}
	want := strings.Trim(etag, "\"")
	for _, p := range strings.Split(in, ",") {
		p = strings.TrimSpace(p)
		if p == "*" || strings.Trim(p, "\"") == "*" {
			return true
		}
		if strings.HasPrefix(p, "W/") {
			p = p[2:]
		}
		if strings.Trim(p, "\"") == want {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := WrapError(err)
	status := apiErr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	reqID := requestIDFromHeader(r)
	if reqID == "" {
		reqID = RequestIDFrom(r.Context())
	}
	if reqID == "" {
		// Reached only for errors outside the /v1/ and /mcp/ handlers (the
		// origin and content-type guards), which run before the transports
		// assign an id.
		reqID = newRequestID()
	}
	w.Header().Set("X-Request-Id", reqID)
	// Server-class failures (5xx) are operator-visible at Error level with
	// their cause; request-class failures are client problems, logged only
	// when debugging.
	if status >= http.StatusInternalServerError {
		slog.Error("api error", "code", apiErr.Code, "status", status, "request_id", reqID, "cause", apiErr.Cause)
	} else {
		slog.Debug("api error", "code", apiErr.Code, "status", status, "request_id", reqID, "cause", apiErr.Cause)
	}
	writeJSONStatus(w, status, map[string]any{"ok": false, "error": apiErr.Public(reqID)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeJSONStatus(w, status, v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
