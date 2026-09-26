package conformance

import (
	"net/http"
	"strings"
	"testing"
)

func TestTheTransportsAgreeOnMalformedBodies(t *testing.T) {
	h := newHarness(t)

	t.Run("a null body is not an empty body", func(t *testing.T) {
		res, body := h.httpCallRaw("list_namespaces", `null`, "application/json")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: only a body with nothing in it is {}: %s", res.StatusCode, body)
		}
		env := envelopeFromString(t, body)
		if env["code"] != "invalid_request" {
			t.Fatalf("code %v, want invalid_request: %s", env["code"], body)
		}
		wantMessage(t, "null body", env["message"].(string), `^request body must be a JSON object, but the request sent null`)
	})

	t.Run("an empty body is still an empty object", func(t *testing.T) {
		for _, body := range []string{"", " ", "\n"} {
			res, out := h.httpCallRaw("list_namespaces", body, "application/json")
			if res.StatusCode != http.StatusOK {
				t.Fatalf("body %q: status %d, want 200: %s", body, res.StatusCode, out)
			}
		}
	})

	t.Run("a non-object body is the same class on both transports", func(t *testing.T) {
		res, body := h.httpCallRaw("query", `[1,2]`, "application/json")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
		}
		wantMessage(t, "non object body", envelopeFromString(t, body)["message"].(string),
			`^request body must be a JSON object, but the request sent an array`)
		mcp := h.mcpCall("query", []any{1, 2})
		if mcp.proto == nil {
			t.Fatalf("MCP must answer non-object arguments as a JSON-RPC protocol error, got %+v", mcp)
		}
		if code := mcp.proto["code"]; code != float64(-32602) {
			t.Fatalf("MCP code %v, want -32602: %+v", code, mcp.proto)
		}
		if msg, _ := mcp.proto["message"].(string); !strings.Contains(msg, "must be an object") {
			t.Fatalf("MCP message %q must name the object requirement", msg)
		}
	})

	t.Run("a null arguments member is the same class on both transports", func(t *testing.T) {
		mcp := h.mcpCall("query", nil)
		if mcp.proto == nil {
			t.Fatalf("MCP must reject null arguments as a protocol error, got %+v", mcp)
		}
		if code := mcp.proto["code"]; code != float64(-32602) {
			t.Fatalf("MCP code %v, want -32602: %+v", code, mcp.proto)
		}
	})

	t.Run("a miscased key is an unknown field, not a type mismatch", func(t *testing.T) {
		for _, key := range []string{"NAMESPACE", "Namespace", "namespacE"} {
			res, body := h.httpCallRaw("query", `{"`+key+`":"decf","sql":"SELECT 1"}`, "application/json")
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s: status %d, want 400: %s", key, res.StatusCode, body)
			}
			msg := envelopeFromString(t, body)["message"].(string)
			wantMessage(t, "miscased "+key, msg, `^unknown field "`+key+`" on operation query`)
			if strings.Contains(msg, "must be a") {
				t.Fatalf("%s must not be reported as a type mismatch: %q", key, msg)
			}
			mcp := h.mcpCall("query", map[string]any{key: "decf", "sql": "SELECT 1"})
			if !mcp.isError() {
				t.Fatalf("MCP %s must fail: %+v", key, mcp)
			}
			wantMessage(t, "mcp miscased "+key, mcp.toolError()["message"].(string), `^unknown field "`+key+`" on operation query`)
		}
	})

	t.Run("an unknown field wins over a type mismatch in either key order", func(t *testing.T) {
		for _, body := range []string{`{"sql":1,"bogus":2}`, `{"bogus":2,"sql":1}`} {
			res, out := h.httpCallRaw("query", body, "application/json")
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s: status %d, want 400: %s", body, res.StatusCode, out)
			}
			msg := envelopeFromString(t, out)["message"].(string)
			wantMessage(t, "key order "+body, msg, `^unknown field "bogus" on operation query`)
			if strings.Contains(msg, "must be a") {
				t.Fatalf("%s: the error class must not depend on key order: %q", body, msg)
			}
		}
	})

	t.Run("a miscased key nested in the body is an unknown field too", func(t *testing.T) {
		res, out := h.httpCallRaw("create_table", `{"namespace":"decf","table":"t","fields":[{"nAmE":"a","type":"string"}]}`, "application/json")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, out)
		}
		wantMessage(t, "nested miscased key", envelopeFromString(t, out)["message"].(string), `^unknown field "nAmE" on operation create_table`)
	})

	t.Run("a wrongly typed value on a real field is still a type mismatch", func(t *testing.T) {
		res, out := h.httpCallRaw("query", `{"namespace":"decf","sql":1}`, "application/json")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", res.StatusCode, out)
		}
		wantMessage(t, "type mismatch", envelopeFromString(t, out)["message"].(string),
			`^field "sql" must be a string, but the request sent a number`)
	})
}
