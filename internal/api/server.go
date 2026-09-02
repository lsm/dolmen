package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
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

var Ops = map[string]OpDef{
	"list_tables": {
		Description: "List tables in a namespace.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"namespace": prop("string", "Namespace to list tables in")},
			"required":   []string{"namespace"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req nsReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			tables, err := s.st.ListTables(ctx, normNS(req.Namespace))
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"tables": tables}, nil
		},
	},
	"describe_table": {
		Description: "Get the schema, version, and row count of a table.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": prop("string", "Namespace of the table"),
				"table":     prop("string", "Table name"),
			},
			"required": []string{"namespace", "table"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req tableReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			sc, count, err := s.st.DescribeTable(ctx, normNS(req.Namespace), normTable(req.Table))
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"table": sc, "row_count": count}, nil
		},
	},
	"create_table": {
		Description: "Create a table with typed fields. Types: string, text, number, boolean, timestamp, json, vector. " +
			"Annotations: fulltext=true indexes a string/text field for full-text search; type=vector stores caller-provided " +
			"embeddings (dim required); vectorize=true on a string/text field makes the server embed that field automatically " +
			"for vector search. Consider infer_schema first when starting from sample records.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": prop("string", "Namespace to create the table in"),
				"table":     prop("string", "Table name (lowercase, [a-z0-9_])"),
				"fields": map[string]any{
					"type":        "array",
					"description": "Field definitions",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":      prop("string", "Field name (lowercase, [a-z0-9_])"),
							"type":      prop("string", "One of: string, text, number, boolean, timestamp, json, vector"),
							"fulltext":  prop("boolean", "Index this field for full-text search (string/text only)"),
							"vectorize": prop("boolean", "Server embeds this text field automatically (string/text only, one per table)"),
							"dim":       prop("number", "Dimension for vector fields"),
							"required":  prop("boolean", "Reject inserts that omit this field"),
						},
						"required": []string{"name"},
					},
				},
			},
			"required": []string{"namespace", "table", "fields"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req createTableReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			sc, err := s.st.CreateTable(ctx, normNS(req.Namespace), normTable(req.Table), req.Fields)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"table": sc}, nil
		},
	},
	"infer_schema": {
		Description: "Propose table fields from sample JSON records (types, fulltext and timestamp detection). " +
			"Review the proposal, adjust, then call create_table. Nothing is created by this call.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"samples": map[string]any{
					"type":        "array",
					"description": "Sample records (JSON objects)",
					"items":       map[string]any{"type": "object"},
				},
			},
			"required": []string{"samples"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req inferReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			if len(req.Samples) == 0 {
				return nil, badRequest("samples must not be empty")
			}
			if len(req.Samples) > 50 {
				return nil, badRequest("too many samples: %d > 50", len(req.Samples))
			}
			return map[string]any{"fields": schema.InferFields(req.Samples)}, nil
		},
	},
	"insert": {
		Description: "Insert one or more records (JSON objects) into a table. Unknown keys are rejected; " +
			"missing required fields are rejected. Full-text and vector indexes update automatically; " +
			"vectorized fields are embedded by the server.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": prop("string", "Namespace of the table"),
				"table":     prop("string", "Table name"),
				"records": map[string]any{
					"type":        "array",
					"description": "Records to insert (JSON objects keyed by field name)",
					"items":       map[string]any{"type": "object"},
				},
			},
			"required": []string{"namespace", "table", "records"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req insertReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			ids, err := s.st.Insert(ctx, normNS(req.Namespace), normTable(req.Table), req.Records, s.embedder())
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"ids": ids, "inserted": len(ids)}, nil
		},
	},
	"query": {
		Description: "Run a read-only SQL statement (SELECT or WITH only) against one namespace. " +
			"Use table and column names from list_tables/describe_table. Bind parameters with ? and pass args. " +
			"Vector columns come back as base64 strings; id and created_at are included in SELECT *.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": prop("string", "Namespace to query"),
				"sql":       prop("string", "Read-only SQL (SELECT/WITH)"),
				"args": map[string]any{
					"type":        "array",
					"description": "Optional bind parameters for ? placeholders",
					"items":       map[string]any{},
				},
			},
			"required": []string{"namespace", "sql"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req queryReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			rows, truncated, err := s.st.Query(ctx, normNS(req.Namespace), req.SQL, req.Args)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"rows": rows, "row_count": len(rows), "truncated": truncated}, nil
		},
	},
	"search_fulltext": {
		Description: "Full-text search over fields marked fulltext, using SQLite FTS5 MATCH syntax " +
			"(e.g. \"payment\", \"'credit refund'\", \"status:ok AND retry\"). Returns matching records ordered by relevance.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": prop("string", "Namespace of the table"),
				"table":     prop("string", "Table name"),
				"query":     prop("string", "FTS5 MATCH expression"),
				"limit":     prop("number", "Max results (default 10, max 200)"),
			},
			"required": []string{"namespace", "table", "query"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req ftsReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			if req.Query == "" {
				return nil, badRequest("query must not be empty")
			}
			results, truncated, err := s.st.SearchFulltext(ctx, normNS(req.Namespace), normTable(req.Table), req.Query, limit(req.Limit))
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"results": results, "truncated": truncated}, nil
		},
	},
	"search_vector": {
		Description: "Nearest-neighbor vector search. Pass text (the server embeds it) or a raw vector. " +
			"column is optional: defaults to the auto-embedding of a vectorized field, else the first vector field. " +
			"Results carry _score (cosine similarity, higher is closer).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": prop("string", "Namespace of the table"),
				"table":     prop("string", "Table name"),
				"text":      prop("string", "Query text; the server embeds it (requires an embedding provider)"),
				"vector": map[string]any{
					"type":        "array",
					"description": "Raw query vector",
					"items":       map[string]any{"type": "number"},
				},
				"column": prop("string", "Vector column to search (optional)"),
				"limit":  prop("number", "Max results (default 10, max 200)"),
			},
			"required": []string{"namespace", "table"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req vecReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			var vec []float32
			switch {
			case req.Text != "":
				if err := s.st.ValidateVectorSearch(ctx, normNS(req.Namespace), normTable(req.Table),
					strings.ToLower(strings.TrimSpace(req.Column)), s.emb.Identity()); err != nil {
					return nil, wrapStoreErr(err)
				}
				vecs, err := s.emb.Embed(ctx, []string{req.Text})
				if err != nil {
					return nil, wrapStoreErr(err)
				}
				vec = vecs[0]
			case len(req.Vector) > 0:
				vec = make([]float32, len(req.Vector))
				for i, x := range req.Vector {
					if math.IsNaN(x) || math.Abs(x) > math.MaxFloat32 {
						return nil, badRequest("vector entry %d is outside the float32 range", i)
					}
					vec[i] = float32(x)
				}
			default:
				return nil, badRequest("pass either text or vector")
			}
			results, truncated, err := s.st.SearchVector(ctx, normNS(req.Namespace), normTable(req.Table),
				strings.ToLower(strings.TrimSpace(req.Column)), vec, s.emb.Identity(), limit(req.Limit))
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"results": results, "truncated": truncated}, nil
		},
	},
	"delete": {
		Description: "Delete rows matching a SQL WHERE expression (e.g. \"status = 'done'\" or \"id IN (3, 7)\"). " +
			"Rows are also removed from search indexes. The filter is required; pass \"1=1\" to empty the table.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": prop("string", "Namespace of the table"),
				"table":     prop("string", "Table name"),
				"filter":    prop("string", "SQL WHERE expression selecting rows to delete"),
				"args": map[string]any{
					"type":        "array",
					"description": "Optional bind parameters for ? placeholders",
					"items":       map[string]any{},
				},
			},
			"required": []string{"namespace", "table", "filter"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req deleteReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			deleted, err := s.st.Delete(ctx, normNS(req.Namespace), normTable(req.Table), req.Filter, req.Args)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"deleted": deleted}, nil
		},
	},
	"migrate": {
		Description: "Evolve a table schema: add_field, rename_field, drop_field, set_fulltext, set_vectorize. " +
			"Bumps the schema version and records the change. Adding fulltext rebuilds the search index; " +
			"enabling vectorize backfills embeddings for existing rows.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": prop("string", "Namespace of the table"),
				"table":     prop("string", "Table name"),
				"changes": map[string]any{
					"type":        "array",
					"description": "Ordered list of changes",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"op":    prop("string", "add_field | rename_field | drop_field | set_fulltext | set_vectorize"),
							"field": map[string]any{"type": "object", "description": "Field definition for add_field"},
							"from":  prop("string", "Current name (rename_field)"),
							"to":    prop("string", "New name (rename_field)"),
							"name":  prop("string", "Field name (drop_field, set_fulltext, set_vectorize)"),
							"value": prop("boolean", "Flag value (set_fulltext, set_vectorize)"),
						},
						"required": []string{"op"},
					},
				},
			},
			"required": []string{"namespace", "table", "changes"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req migrateReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			sc, err := s.st.Migrate(ctx, normNS(req.Namespace), normTable(req.Table), req.Changes, s.embedder())
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"table": sc}, nil
		},
	},
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

type insertReq struct {
	Namespace string           `json:"namespace"`
	Table     string           `json:"table"`
	Records   []map[string]any `json:"records"`
}

type queryReq struct {
	Namespace string `json:"namespace"`
	SQL       string `json:"sql"`
	Args      []any  `json:"args"`
}

type ftsReq struct {
	Namespace string `json:"namespace"`
	Table     string `json:"table"`
	Query     string `json:"query"`
	Limit     int    `json:"limit"`
}

type vecReq struct {
	Namespace string    `json:"namespace"`
	Table     string    `json:"table"`
	Column    string    `json:"column"`
	Text      string    `json:"text"`
	Vector    []float64 `json:"vector"`
	Limit     int       `json:"limit"`
}

type deleteReq struct {
	Namespace string `json:"namespace"`
	Table     string `json:"table"`
	Filter    string `json:"filter"`
	Args      []any  `json:"args"`
}

type migrateReq struct {
	Namespace string          `json:"namespace"`
	Table     string          `json:"table"`
	Changes   []schema.Change `json:"changes"`
}

type inferReq struct {
	Samples []map[string]any `json:"samples"`
}

func decode(body []byte, v any) error {
	if len(body) == 0 {
		return badRequest("empty request body")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return badRequest("invalid JSON: %v", err)
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
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
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
		if r.Method != http.MethodPost {
			writeError(w, &Error{Status: http.StatusMethodNotAllowed, Message: "use POST"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
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
