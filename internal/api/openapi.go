package api

import (
	"net/http"

	"github.com/lsm/dolmen/internal/schema"
)

const (
	openAPIVersion = "3.1.0"
	apiVersion     = "0.1.0"
)

var outputSchemas = map[string]map[string]any{
	"list_tables":     objectSchema(false, map[string]any{"tables": arrayOf(map[string]any{"type": "string"})}, []string{"tables"}),
	"describe_table":  objectSchema(false, map[string]any{"table": ref("TableSchema"), "row_count": integer(0)}, []string{"table", "row_count"}),
	"create_table":    objectSchema(false, map[string]any{"table": ref("TableSchema")}, []string{"table"}),
	"infer_schema":    objectSchema(false, map[string]any{"fields": arrayOf(ref("Field"))}, []string{"fields"}),
	"insert":          objectSchema(false, map[string]any{"ids": arrayOf(integer(1)), "inserted": integer(0), "replayed": propBool()}, []string{"ids", "inserted"}),
	"upsert_by_key":   objectSchema(false, map[string]any{"ids": arrayOf(integer(1)), "inserted": integer(0), "updated": integer(0)}, []string{"ids", "inserted", "updated"}),
	"query":           objectSchema(false, map[string]any{"rows": arrayOf(ref("Row")), "row_count": integer(0), "truncated": propBool()}, []string{"rows", "row_count", "truncated"}),
	"search_fulltext": objectSchema(false, map[string]any{"results": arrayOf(ref("Row")), "truncated": propBool()}, []string{"results", "truncated"}),
	"search_vector":   objectSchema(false, map[string]any{"results": arrayOf(ref("Row")), "truncated": propBool()}, []string{"results", "truncated"}),
	"delete":          objectSchema(false, map[string]any{"deleted": integer(0)}, []string{"deleted"}),
	"update":          objectSchema(false, map[string]any{"updated": integer(0)}, []string{"updated"}),
	"upsert":          objectSchema(false, map[string]any{"inserted": propBool(), "updated": integer(0), "id": integer(1)}, []string{"inserted", "updated"}),
	"migrate":         objectSchema(false, map[string]any{"table": ref("TableSchema")}, []string{"table"}),
}

func init() {
	for name, def := range Ops {
		if sc, ok := outputSchemas[name]; ok {
			def.OutputSchema = sc
			Ops[name] = def
		}
	}
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, &Error{Status: http.StatusMethodNotAllowed, Message: "use GET"})
		return
	}
	writeJSON(w, http.StatusOK, s.OpenAPIDoc())
}

// OpenAPIDoc returns an OpenAPI 3.1.0 description of the /v1 HTTP API,
// generated from the live Ops registry so it stays in sync with the handlers.
func (s *Server) OpenAPIDoc() map[string]any {
	paths := map[string]any{}
	for _, name := range OpNames() {
		def := Ops[name]
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

	return map[string]any{
		"openapi": openAPIVersion,
		"info": map[string]any{
			"title":       "Dolmen HTTP API",
			"version":     apiVersion,
			"description": "Structured tables, full-text search, and vector search over a single-binary data layer.",
		},
		"servers": []map[string]any{
			map[string]any{"url": "/"},
		},
		"paths":      paths,
		"components": components(),
	}
}

func components() map[string]any {
	fieldTypeEnum := []schema.FieldType{
		schema.String, schema.Text, schema.Number, schema.Boolean,
		schema.Timestamp, schema.JSON, schema.Vector,
	}
	return map[string]any{
		"schemas": map[string]any{
			"ErrorEnvelope": objectSchema(false, map[string]any{
				"ok":    map[string]any{"const": false},
				"error": stringProp(""),
			}, []string{"ok", "error"}),
			"Field": objectSchema(false, map[string]any{
				"name":      stringProp(`^[a-z][a-z0-9_]{0,63}$`),
				"type":      enumProp(fieldTypeEnum),
				"fulltext":  propBool(),
				"vectorize": propBool(),
				"dim":       intProp(1, schema.MaxVectorDim),
				"required":  propBool(),
			}, []string{"name", "type"}),
			"TableSchema": objectSchema(false, map[string]any{
				"namespace":   stringProp(`^[a-z0-9][a-z0-9_-]{0,63}$`),
				"name":        stringProp(`^[a-z][a-z0-9_]{0,63}$`),
				"version":     intProp(1, 0),
				"fields":      arrayOf(ref("Field")),
				"embed_space": stringProp(""),
				"embed_dim":   intProp(0, 0),
			}, []string{"namespace", "name", "version", "fields"}),
			"Row": map[string]any{
				"type":        "object",
				"description": "A result row keyed by column or field name; values are typed per the table schema.",
				"additionalProperties": true,
			},
		},
	}
}

func requestBody(inputSchema map[string]any) map[string]any {
	return map[string]any{
		"required": true,
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
