package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/lsm/dolmen/internal/api"
)

const (
	protocolVersion     = "2025-06-18"
	serverName          = "dolmen"
	serverVersion       = "0.1.0"
	jsonRPCParseError   = -32700
	jsonRPCMethodError  = -32601
	jsonRPCInvalidReq   = -32600
	jsonRPCInvalidParam = -32602
)

type Server struct {
	api     *api.Server
	origins map[string]bool
}

// toolAnnotations carries the MCP annotations (client-side UI hints) for each
// registry op. Titles and hints describe the op, not the transport. Read ops
// that touch the store still create the namespace db on first use (Store.ns
// opens and initializes <namespace>.db), so they must not claim readOnlyHint
// until that changes; infer_schema is pure and does. Ops that can send text to
// a configured remote embedding provider (insert on vectorized fields,
// search_vector by text, migrate when enabling vectorize, and the write ops
// that re-embed) are open-world: the client cannot know whether the deployment
// embeds locally. Provider-capable ops are not idempotent either — retries
// re-hit (and re-bill) the endpoint even when the row state would converge.
// Every write except a plain insert is destructive: it can overwrite or drop
// existing data, and filter-driven writes (delete, update, upsert) can walk
// new rows on retry via a non-deterministic WHERE.
var toolAnnotations = map[string]map[string]any{
	"list_tables":     {"title": "List tables", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
	"describe_table":  {"title": "Describe table", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
	"create_table":    {"title": "Create table", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false},
	"infer_schema":    {"title": "Infer schema", "readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
	"insert":          {"title": "Insert records", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": true},
	"upsert_by_key":   {"title": "Upsert by natural key", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true},
	"query":           {"title": "Query", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
	"search_fulltext": {"title": "Full-text search", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
	"search_vector":   {"title": "Vector search", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": true},
	"delete":          {"title": "Delete rows", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false},
	"update":          {"title": "Update rows", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true},
	"upsert":          {"title": "Upsert by filter", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true},
	"migrate":         {"title": "Migrate table", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true},
	"list_migrations": {"title": "List migrations", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
}

func New(a *api.Server, extraOrigins []string) *Server {
	origins := map[string]bool{}
	for _, o := range extraOrigins {
		origins[strings.ToLower(strings.TrimRight(strings.TrimSpace(o), "/"))] = true
	}
	return &Server{api: a, origins: origins}
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("MCP-Protocol-Version", protocolVersion)
	if origin := r.Header.Get("Origin"); origin != "" {
		allowed := s.origins[strings.ToLower(strings.TrimRight(origin, "/"))]
		if !allowed {
			if u, err := url.Parse(origin); err == nil {
				switch strings.ToLower(u.Hostname()) {
				case "localhost", "127.0.0.1", "::1":
					allowed = true
				}
			}
		}
		if !allowed {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
	}
	if r.Method == http.MethodPost {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body exceeds the 32 MiB limit", http.StatusRequestEntityTooLarge)
			return
		}
		writeRPCError(w, nil, jsonRPCParseError, "cannot read request body")
		return
	}
	var probe any
	probeDec := json.NewDecoder(bytes.NewReader(body))
	probeDec.UseNumber()
	if err := probeDec.Decode(&probe); err != nil {
		writeRPCError(w, nil, jsonRPCParseError, "invalid JSON")
		return
	}
	if err := probeDec.Decode(&struct{}{}); err != io.EOF {
		writeRPCError(w, nil, jsonRPCParseError, "trailing content after JSON body")
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		writeRPCError(w, nil, jsonRPCInvalidReq, "expected a JSON-RPC 2.0 request object")
		return
	}
	if msg.JSONRPC != "2.0" || msg.Method == "" {
		writeRPCError(w, validID(msg.ID), jsonRPCInvalidReq, "expected a JSON-RPC 2.0 request")
		return
	}
	if msg.Method != "initialize" {
		if pv := r.Header.Get("MCP-Protocol-Version"); pv != "" && pv != protocolVersion {
			http.Error(w, "unsupported MCP-Protocol-Version", http.StatusBadRequest)
			return
		}
	}
	if len(msg.ID) > 0 && validID(msg.ID) == nil {
		writeRPCError(w, nil, jsonRPCInvalidReq, "request id must be a string or number")
		return
	}
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := s.handle(r.Context(), msg)
	if rpcErr != nil {
		writeRPCError(w, msg.ID, rpcErr.Code, rpcErr.Message)
		return
	}
	writeRPCResult(w, msg.ID, result)
}

type rpcErr struct {
	Code    int
	Message string
}

func (s *Server) handle(ctx context.Context, msg rpcMessage) (any, *rpcErr) {
	switch msg.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		initProbe, e := ensureObjectParams(msg.Params, "initialize")
		if e != nil {
			return nil, e
		}
		if ci, ok := initProbe["clientInfo"].(map[string]any); ok {
			if title, ok := ci["title"]; ok {
				if _, isStr := title.(string); !isStr {
					return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "clientInfo title must be a string"}
				}
			}
		}
		initDec := json.NewDecoder(bytes.NewReader(msg.Params))
		initDec.UseNumber()
		if err := initDec.Decode(&params); err != nil {
			return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "invalid initialize params"}
		}
		if params.ProtocolVersion == "" {
			return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "initialize params must carry the client protocolVersion"}
		}
		if params.Capabilities == nil {
			return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "initialize params must carry the client capabilities object"}
		}
		if rpcErr := validateCapabilityShapes(params.Capabilities); rpcErr != nil {
			return nil, rpcErr
		}
		if params.ClientInfo.Name == "" || params.ClientInfo.Version == "" {
			return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "initialize params must carry clientInfo with name and version"}
		}
		pv := protocolVersion
		if params.ProtocolVersion == protocolVersion {
			pv = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		}, nil
	case "ping":
		if _, e := ensureObjectParams(msg.Params, "ping"); e != nil {
			return nil, e
		}
		return map[string]any{}, nil
	case "tools/list":
		probe, e := ensureObjectParams(msg.Params, "tools/list")
		if e != nil {
			return nil, e
		}
		if c, ok := probe["cursor"]; ok {
			if _, isStr := c.(string); !isStr {
				return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "tools/list cursor must be a string"}
			}
		}
		tools := make([]map[string]any, 0)
		for _, name := range api.OpNames() {
			def := api.Ops[name]
			tool := map[string]any{
				"name":        name,
				"description": def.Description,
				"inputSchema": def.InputSchema,
			}
			if def.OutputSchema != nil {
				tool["outputSchema"] = def.OutputSchema
			}
			if ann, ok := toolAnnotations[name]; ok {
				tool["annotations"] = ann
			}
			tools = append(tools, tool)
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "invalid tools/call params"}
		}
		if params.Name == "" {
			return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "tools/call params must carry the tool name"}
		}
		if _, e := ensureObjectParams(msg.Params, "tools/call"); e != nil {
			return nil, e
		}
		args := params.Arguments
		if len(bytes.TrimSpace(args)) == 0 {
			args = json.RawMessage("{}")
		} else {
			argDec := json.NewDecoder(bytes.NewReader(args))
			argDec.UseNumber()
			var argProbe map[string]any
			if err := argDec.Decode(&argProbe); err != nil || argProbe == nil {
				return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "tools/call arguments must be an object"}
			}
		}
		if _, known := api.Ops[params.Name]; !known {
			return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: fmt.Sprintf("unknown tool %q", params.Name)}
		}
		res, err := s.api.Dispatch(ctx, params.Name, args)
		if err != nil {
			return toolError(fmt.Sprintf("error: %s", err.Error())), nil
		}
		return toolResult(res), nil
	default:
		return nil, &rpcErr{Code: jsonRPCMethodError, Message: fmt.Sprintf("unknown method %q", msg.Method)}
	}
}

func validID(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var probe any
	if err := dec.Decode(&probe); err != nil {
		return nil
	}
	switch probe.(type) {
	case string, json.Number:
		return raw
	}
	return nil
}

func validateCapabilityShapes(caps map[string]any) *rpcErr {
	badShape := &rpcErr{Code: jsonRPCInvalidParam, Message: "malformed capability declaration"}
	if v, ok := caps["roots"]; ok {
		m, isObj := v.(map[string]any)
		if !isObj {
			return badShape
		}
		if lc, ok := m["listChanged"]; ok {
			if _, isBool := lc.(bool); !isBool {
				return badShape
			}
		}
	}
	for _, key := range []string{"sampling", "elicitation", "experimental"} {
		if v, ok := caps[key]; ok {
			m, isObj := v.(map[string]any)
			if !isObj {
				return badShape
			}
			if key == "experimental" {
				for _, ev := range m {
					if _, isObj := ev.(map[string]any); !isObj {
						return badShape
					}
				}
			}
		}
	}
	return nil
}

func ensureObjectParams(raw []byte, what string) (map[string]any, *rpcErr) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var probe map[string]any
	if err := dec.Decode(&probe); err != nil || probe == nil {
		return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: fmt.Sprintf("invalid %s params", what)}
	}
	if meta, ok := probe["_meta"]; ok {
		m, isObj := meta.(map[string]any)
		if !isObj {
			return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: fmt.Sprintf("invalid %s _meta", what)}
		}
		if pt, ok := m["progressToken"]; ok {
			switch pt.(type) {
			case string, json.Number:
			default:
				return nil, &rpcErr{Code: jsonRPCInvalidParam, Message: "_meta progressToken must be a string or number"}
			}
		}
	}
	return probe, nil
}

// toolResult shapes a successful call: the result object travels only as
// structuredContent. The spec keeps content mandatory (an array of content
// blocks) but does not require mirroring the payload as text — dolmen has no
// legacy clients to carry, and a text mirror would double every result.
func toolResult(structured any) map[string]any {
	return map[string]any{
		"content":           []map[string]any{},
		"structuredContent": structured,
		"isError":           false,
	}
}

func toolError(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": true,
	}
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
	w.Header().Set("Content-Type", "application/json")
	if code == jsonRPCParseError || code == jsonRPCInvalidReq {
		w.WriteHeader(http.StatusBadRequest)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}
