package api

import (
	"net/http"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

const (
	openAPIVersion = "3.1.0"
	apiVersion     = "0.3.0"
)

var writeDataSchema = objectSchema(false, map[string]any{
	"ids":      arrayOf(integer(1)),
	"inserted": integer(0),
	"updated":  integer(0),
}, []string{"ids", "inserted", "updated"})

var capabilitiesOutSchema = objectSchema(false, map[string]any{
	"vector_execution": map[string]any{"type": "string", "enum": []string{string(store.VectorExact), string(store.VectorANN)}},
	"ann_recall_bound": map[string]any{
		"anyOf": []any{
			map[string]any{"type": "number"},
			map[string]any{"type": "null"},
		},
	},
	"notifications":  propBool(),
	"subscribe":      propBool(),
	"query_dialect":  stringProp(""),
	"filter_dialect": stringProp(""),
}, []string{"vector_execution", "ann_recall_bound", "notifications", "subscribe", "query_dialect", "filter_dialect"})

var changesOutSchema = objectSchema(false, map[string]any{
	"changes": arrayOf(objectSchema(false, map[string]any{
		"cursor": stringProp(""),
		"table":  stringProp(`^[a-z][a-z0-9_]{0,63}$`),
		"row_id": integer(1),
		"kind":   map[string]any{"type": "string", "enum": []string{"insert", "update", "delete"}},
	}, []string{"cursor", "table", "row_id", "kind"})),
	"next_cursor": stringProp(""),
}, []string{"changes", "next_cursor"})

var outputSchemas = map[string]map[string]any{
	"list_tables":     objectSchema(false, map[string]any{"tables": arrayOf(map[string]any{"type": "string"})}, []string{"tables"}),
	"describe_table":  objectSchema(false, map[string]any{"table": ref("TableSchema"), "row_count": integer(0)}, []string{"table", "row_count"}),
	"create_table":    objectSchema(false, map[string]any{"table": ref("TableSchema")}, []string{"table"}),
	"infer_schema":    objectSchema(false, map[string]any{"fields": arrayOf(ref("Field"))}, []string{"fields"}),
	"insert":          objectSchema(false, map[string]any{"ids": arrayOf(integer(1)), "inserted": integer(0), "replayed": propBool()}, []string{"ids", "inserted"}),
	"upsert_by_key":   writeDataSchema,
	"query":           objectSchema(false, map[string]any{"rows": arrayOf(ref("Row")), "row_count": integer(0), "truncated": propBool()}, []string{"rows", "row_count", "truncated"}),
	"read_rows":       objectSchema(false, map[string]any{"rows": arrayOf(ref("Row")), "row_count": integer(0), "truncated": propBool()}, []string{"rows", "row_count", "truncated"}),
	"capabilities":    capabilitiesOutSchema,
	"search_fulltext": objectSchema(false, map[string]any{"results": arrayOf(ref("Row")), "truncated": propBool()}, []string{"results", "truncated"}),
	"search_vector":   objectSchema(false, map[string]any{"results": arrayOf(ref("Row")), "truncated": propBool()}, []string{"results", "truncated"}),
	"changes_since":   changesOutSchema,
	"wait_for":        changesOutSchema,
	"delete":          objectSchema(false, map[string]any{"deleted": integer(0)}, []string{"deleted"}),
	"update":          objectSchema(false, map[string]any{"updated": integer(0)}, []string{"updated"}),
	"upsert":          writeDataSchema,
	"migrate":         migrateOutSchema(ref("TableSchema"), ref("TableSchema")),
}

func init() {
	for name, def := range Ops {
		if sc, ok := outputSchemas[name]; ok {
			def.OutputSchema = sc
			Ops[name] = def
		}
	}
}

var errorCodes = []ErrorCode{
	ErrCodeInvalid,
	ErrCodeNotFound,
	ErrCodeQuery,
	ErrCodeConflict,
	ErrCodeUnauthorized,
	ErrCodeForbidden,
	ErrCodeEmbedderUnavailable,
	ErrCodeCanceled,
	ErrCodeTimeout,
	ErrCodeInternal,
}

func errorCodeEnum(authOn bool) []string {
	out := make([]string, 0, len(errorCodes))
	for _, code := range errorCodes {
		if code == ErrCodeUnauthorized && !authOn {
			continue
		}
		out = append(out, string(code))
	}
	return out
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, r, &Error{Status: http.StatusMethodNotAllowed, Code: ErrCodeInvalid, Message: "use GET"})
		return
	}
	if !s.UsableHost(r) {
		writeError(w, r, errUnusableHost)
		return
	}
	ctx := s.publicContext(r)
	setPublicURLCacheHeaders(w)
	writeJSON(w, http.StatusOK, s.OpenAPIDoc(ctx.BaseURL))
}

func (s *Server) OpenAPIDoc(baseURL string) map[string]any {
	paths := map[string]any{}
	for _, name := range s.OpNames() {
		def, _ := s.Op(name)
		dataSchema := def.OutputSchema
		if dataSchema == nil {
			dataSchema = map[string]any{"type": "object"}
		}
		paths["/v1/"+name] = map[string]any{
			"post": map[string]any{
				"operationId": name,
				"summary":     def.Description,
				"requestBody": requestBody(def.InputSchema),
				"responses":   opResponses(dataSchema),
			},
		}
	}

	paths["/v1/subscribe"] = map[string]any{
		"get": map[string]any{
			"operationId": "subscribe",
			"summary": "Server-sent events over the namespace change feed: replay from an optional cursor, " +
				"a ready frame at the replay-to-live boundary carrying the cursor a reconnect resumes from, " +
				"then live change frames in commit order, with periodic keepalive comments on an idle stream. " +
				"Unlike the POST operations this is a GET with query parameters and a text/event-stream body; " +
				"a namespace that does not exist is an in-stream not_found error, and the stream never creates one.",
			"parameters": []any{
				queryParam("namespace", "Namespace whose change feed to stream", true, nsProp("")),
				queryParam("table", "Optional table filter: stream only this table's current lifetime", false,
					map[string]any{"type": "string", "pattern": `^[a-z][a-z0-9_]{0,63}$`}),
				queryParam("cursor", "Resume token from a previous frame or page; \"begin\" replays retained history; omitted starts at the current head", false,
					map[string]any{"type": "string", "minLength": 1}),
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "An event stream of ready, change, and error frames",
					"content": map[string]any{
						"text/event-stream": map[string]any{
							"schema": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}

	if _, ok := s.oidc(); ok {
		paths[auth.AuthBeginPath] = map[string]any{
			"get": map[string]any{
				"operationId": "auth_begin",
				"summary": "Start the browser sign-in: answers with a redirect to the identity provider's authorization endpoint, " +
					"carrying PKCE and a single-use state. Unlike the POST operations this is a GET that answers with a redirect or " +
					"an HTML page, never a JSON envelope, and it is unauthenticated. It appears here only when the deployment has an " +
					"identity provider configured.",
				"responses": map[string]any{
					"302": map[string]any{
						"description": "Redirect to the identity provider's authorization endpoint",
						"headers": map[string]any{
							"Location": map[string]any{
								"description": "The identity provider's authorization URL",
								"schema":      map[string]any{"type": "string"},
							},
						},
					},
					"400": htmlResponse("A page naming why the sign-in could not be started"),
					"403": errorResponse("Origin not allowed"),
					"405": errorResponse("Method not allowed"),
				},
			},
		}
		paths[auth.AuthCallbackPath] = map[string]any{
			"get": map[string]any{
				"operationId": "auth_callback",
				"summary": "Finish the browser sign-in: exchanges the authorization code and answers with an HTML page carrying a bearer " +
					"token. Register this path as the redirect URI with the identity provider; people reach it from the provider, not " +
					"from client code. The token is stateless and expires on its own, and the server keeps no session.",
				"parameters": []any{
					queryParam("state", "The single-use state issued by "+auth.AuthBeginPath, false, map[string]any{"type": "string", "minLength": 1}),
					queryParam("code", "The identity provider's authorization code", false, map[string]any{"type": "string", "minLength": 1}),
					queryParam("error", "Set when the identity provider refused the sign-in; no code is exchanged", false, map[string]any{"type": "string"}),
				},
				"responses": map[string]any{
					"200": htmlResponse("A page carrying the bearer token and how long it lasts"),
					"400": htmlResponse("A page naming why the sign-in could not be completed"),
					"403": errorResponse("Origin not allowed"),
					"405": errorResponse("Method not allowed"),
				},
			},
		}
	}

	serverURL := "/"
	if baseURL != "" {
		serverURL = baseURL
	}
	return map[string]any{
		"openapi": openAPIVersion,
		"info": map[string]any{
			"title":       "Dolmen HTTP API",
			"version":     apiVersion,
			"description": "Structured tables, full-text search, and vector search over a single-binary data layer.",
		},
		"servers": []map[string]any{
			map[string]any{"url": serverURL},
		},
		"paths":      paths,
		"components": components(s.authOpsEnabled()),
	}
}

func queryParam(name, desc string, required bool, schema map[string]any) map[string]any {
	return map[string]any{
		"name":        name,
		"in":          "query",
		"required":    required,
		"description": desc,
		"schema":      schema,
	}
}

func components(authOn bool) map[string]any {
	fieldTypeEnum := []schema.FieldType{
		schema.String, schema.Text, schema.Number, schema.Boolean,
		schema.Timestamp, schema.JSON, schema.Vector,
	}
	return map[string]any{
		"schemas": map[string]any{
			"ErrorEnvelope": objectSchema(false, map[string]any{
				"ok": map[string]any{"const": false},
				"error": objectSchema(false, map[string]any{
					"code":       map[string]any{"type": "string", "enum": errorCodeEnum(authOn)},
					"message":    stringProp(""),
					"request_id": stringProp(""),
				}, []string{"code", "message", "request_id"}),
			}, []string{"ok", "error"}),
			"Field": objectSchema(false, map[string]any{
				"name":      stringProp(`^[a-z][a-z0-9_]{0,63}$`),
				"type":      enumProp(fieldTypeEnum),
				"fulltext":  propBool(),
				"vectorize": propBool(),
				"dim":       intProp(1, schema.MaxVectorDim),
				"required":  propBool(),
				"enum": map[string]any{
					"type":        "array",
					"description": "Allowed values for this string field (present when the field has an enum constraint); writes carrying any other value are rejected",
					"items":       map[string]any{"type": "string"},
				},
				"default": map[string]any{"description": "Value stored when an insert omits the field; exactly as declared (present when set) — \"now()\" on a timestamp field stamps the server's current time at each write"},
			}, []string{"name", "type"}),
			"TableSchema": objectSchema(false, map[string]any{
				"namespace":   stringProp(store.NSPathPattern()),
				"name":        stringProp(`^[a-z][a-z0-9_]{0,63}$`),
				"version":     intProp(1, 0),
				"fields":      arrayOf(ref("Field")),
				"embed_space": stringProp(""),
				"embed_dim":   intProp(0, 0),
			}, []string{"namespace", "name", "version", "fields"}),
			"Row": map[string]any{
				"type":                 "object",
				"description":          "A result row keyed by column or field name; values are typed per the table schema.",
				"additionalProperties": true,
			},
		},
	}
}

func requestBody(inputSchema map[string]any) map[string]any {
	bodyRequired := true
	if req, ok := inputSchema["required"].([]string); !ok || len(req) == 0 {
		bodyRequired = false
	}
	return map[string]any{
		"required": bodyRequired,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": inputSchema,
			},
		},
	}
}

func opResponses(dataSchema map[string]any) map[string]any {
	return map[string]any{
		"200": responseOK(dataSchema),
		"400": errorResponse("Bad request"),
		"403": errorResponse("Origin not allowed"),
		"404": errorResponse("Not found"),
		"405": errorResponse("Method not allowed"),
		"413": errorResponse("Payload too large"),
		"415": errorResponse("Unsupported media type"),
		"500": errorResponse("Internal server error"),
		"503": errorResponse("Embedding provider unavailable (the local model could not be loaded or downloaded); vectorized writes and text searches only"),
	}
}

func htmlResponse(description string) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"text/html": map[string]any{
				"schema": map[string]any{"type": "string"},
			},
		},
	}
}

func responseOK(dataSchema map[string]any) map[string]any {
	return map[string]any{
		"description": "Success",
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": successEnvelope(dataSchema),
			},
		},
	}
}

func ref(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func arrayOf(items map[string]any) map[string]any {
	return map[string]any{
		"type":  "array",
		"items": items,
	}
}

func integer(minimum int) map[string]any {
	s := map[string]any{"type": "integer", "format": "int64"}
	if minimum != 0 {
		s["minimum"] = minimum
	} else {
		s["minimum"] = 0
	}
	return s
}

func propBool() map[string]any {
	return map[string]any{"type": "boolean"}
}

func stringProp(pattern string) map[string]any {
	s := map[string]any{"type": "string"}
	if pattern != "" {
		s["pattern"] = pattern
	}
	return s
}

func enumProp(enum []schema.FieldType) map[string]any {
	return map[string]any{
		"type": "string",
		"enum": enum,
	}
}

func intProp(minimum, maximum int) map[string]any {
	s := map[string]any{"type": "integer", "minimum": minimum}
	if maximum > 0 {
		s["maximum"] = maximum
	}
	return s
}

func objectSchema(additionalProperties bool, properties map[string]any, required []string) map[string]any {
	s := map[string]any{
		"type":                 "object",
		"additionalProperties": additionalProperties,
		"properties":           properties,
	}
	if required != nil {
		s["required"] = required
	}
	return s
}

func successEnvelope(dataSchema map[string]any) map[string]any {
	return objectSchema(false, map[string]any{
		"ok":   map[string]any{"const": true},
		"data": dataSchema,
	}, []string{"ok", "data"})
}

func errorResponse(description string) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": ref("ErrorEnvelope"),
			},
		},
	}
}
