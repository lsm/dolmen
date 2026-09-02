package api

import (
	"context"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

var Ops = map[string]OpDef{
	"list_tables": {
		Description: "List tables in a namespace.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{"namespace": nsProp("Namespace to list tables in")},
			"required":             []string{"namespace"},
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
			if tables == nil {
				tables = []string{}
			}
			return map[string]any{"tables": tables}, nil
		},
	},
	"describe_table": {
		Description: "Get the schema, version, and row count of a table.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     tableProp("Table name"),
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
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace to create the table in"),
				"table":     tableProp("Table name (lowercase [a-z0-9_]; no sqlite_ prefix or __fts)"),
				"fields": map[string]any{
					"type":        "array",
					"description": "Field definitions",
					"minItems":    1,
					"maxItems":    store.MaxFieldsPerTable,
					"uniqueItems": true,
					"not": map[string]any{
						"contains": map[string]any{
							"properties": map[string]any{"vectorize": map[string]any{"const": true}},
							"required":   []string{"vectorize"},
						},
						"minContains": 2,
					},
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]any{
							"name": map[string]any{
								"type":        "string",
								"description": "Field name (lowercase, [a-z0-9_], max 64 chars)",
								"pattern":     `^[a-z][a-z0-9_]{0,63}$`,
								"not": map[string]any{
									"enum": []string{"id", "created_at", "_embedding", "_score", "_rank", "rowid"},
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
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"samples": map[string]any{
					"type":        "array",
					"description": "Sample records (JSON objects)",
					"items":       map[string]any{"type": "object"},
					"minItems":    1,
					"maxItems":    50,
				},
			},
			"required": []string{"samples"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req inferReq
			if err := decodeData(body, &req); err != nil {
				return nil, err
			}
			if len(req.Samples) == 0 {
				return nil, badRequest("samples must not be empty")
			}
			if len(req.Samples) > 50 {
				return nil, badRequest("too many samples: %d > 50", len(req.Samples))
			}
			fields := schema.InferFields(req.Samples)
			if fields == nil {
				fields = []schema.Field{}
			}
			return map[string]any{"fields": fields}, nil
		},
	},
}
