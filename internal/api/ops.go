package api

import (
	"context"

	"github.com/lsm/dolmen/internal/schema"
)

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
			if tables == nil {
				tables = []string{}
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
							"dim":       prop("integer", "Dimension for vector fields"),
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
			fields := schema.InferFields(req.Samples)
			if fields == nil {
				fields = []schema.Field{}
			}
			return map[string]any{"fields": fields}, nil
		},
	},
}
