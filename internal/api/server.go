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
	"sort"
	"strings"

	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type Server struct {
	st  *store.Store
	emb embed.Provider
}

func New(st *store.Store, emb embed.Provider) *Server {
	return &Server{st: st, emb: emb}
}

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func badRequest(format string, args ...any) error {
	return &Error{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &Error{Status: http.StatusNotFound, Message: fmt.Sprintf(format, args...)}
}

func internal(err error) error {
	return &Error{Status: http.StatusInternalServerError, Message: err.Error()}
}

func wrapStoreErr(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	if errors.Is(err, store.ErrNotFound) {
		return &Error{Status: http.StatusNotFound, Message: err.Error()}
	}
	if errors.Is(err, store.ErrInvalid) {
		return &Error{Status: http.StatusBadRequest, Message: err.Error()}
	}
	return internal(err)
}

type OpDef struct {
	Description string
	InputSchema map[string]any
	Func        func(ctx context.Context, s *Server, body []byte) (any, error)
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

func fieldItemSchema(desc string) map[string]any {
	return map[string]any{
		"description":          desc,
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
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
		},
		"required": []string{"name"},
		"allOf": []any{
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
		},
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

func rejectNulls(path string, v any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if val == nil {
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
				writeJSONStatus(w, http.StatusForbidden, map[string]any{"ok": false, "error": "origin not allowed"})
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version, MCP-Session-Id")
				w.Header().Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if r.Method == http.MethodPost {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || strings.ToLower(mt) != "application/json" {
				writeJSONStatus(w, http.StatusUnsupportedMediaType, map[string]any{"ok": false, "error": "content-type must be application/json"})
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
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, "/v1/")
		if op == "" || strings.Contains(op, "/") {
			writeError(w, notFound("unknown operation"))
			return
		}
		if _, ok := Ops[op]; !ok {
			writeError(w, notFound("unknown operation"))
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, &Error{Status: http.StatusMethodNotAllowed, Message: "use POST"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, &Error{Status: http.StatusRequestEntityTooLarge, Message: "request body exceeds the 32 MiB limit"})
				return
			}
			writeError(w, badRequest("cannot read body: %v", err))
			return
		}
		res, err := s.Dispatch(r.Context(), op, body)
		if err != nil {
			slog.Debug("op failed", "op", op, "err", err)
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": res})
	})
	return mux
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var apiErr *Error
	if errors.As(err, &apiErr) {
		status = apiErr.Status
	}
	writeJSONStatus(w, status, map[string]any{"ok": false, "error": err.Error()})
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
