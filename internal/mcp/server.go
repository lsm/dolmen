package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/lsm/dolmen/internal/api"
)

const (
	protocolVersion    = "2025-06-18"
	serverName         = "dolmen"
	serverVersion      = "0.1.0"
	jsonRPCParseError  = -32700
	jsonRPCMethodError = -32601
	jsonRPCInvalidReq  = -32600
)

type Server struct {
	api *api.Server
}

func New(a *api.Server) *Server {
	return &Server{api: a}
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("MCP-Protocol-Version", protocolVersion)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeRPCError(w, nil, jsonRPCParseError, "cannot read request body")
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		writeRPCError(w, nil, jsonRPCParseError, "invalid JSON")
		return
	}
	if msg.JSONRPC != "2.0" || msg.Method == "" {
		writeRPCError(w, msg.ID, jsonRPCInvalidReq, "expected a JSON-RPC 2.0 request")
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
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &params)
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
		return map[string]any{}, nil
	case "tools/list":
		tools := make([]map[string]any, 0)
		for _, name := range api.OpNames() {
			def := api.Ops[name]
			tools = append(tools, map[string]any{
				"name":        name,
				"description": def.Description,
				"inputSchema": def.InputSchema,
			})
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil, &rpcErr{Code: jsonRPCParseError, Message: "invalid tools/call params"}
		}
		args := params.Arguments
		if len(bytes.TrimSpace(args)) == 0 {
			args = json.RawMessage("{}")
		}
		res, err := s.api.Dispatch(ctx, params.Name, args)
		if err != nil {
			return toolResult(fmt.Sprintf("error: %s", err.Error()), true), nil
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if mErr := enc.Encode(res); mErr != nil {
			return toolResult(fmt.Sprintf("error: cannot encode result: %s", mErr.Error()), true), nil
		}
		return toolResult(buf.String(), false), nil
	default:
		return nil, &rpcErr{Code: jsonRPCMethodError, Message: fmt.Sprintf("unknown method %q", msg.Method)}
	}
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
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
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}
