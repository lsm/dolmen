package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

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
				"table":     tableProp("Table name (lowercase [a-z0-9_]; no sqlite_/pragma_ prefix, __fts, or dbstat)"),
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
					"items": fieldItemSchema("Field definition"),
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
			for i, s := range req.Samples {
				if s == nil {
					return nil, badRequest("samples[%d] must be an object, not null", i)
				}
			}
			fields := schema.InferFields(req.Samples)
			if fields == nil {
				fields = []schema.Field{}
			}
			return map[string]any{"fields": fields}, nil
		},
	},
	"insert": {
		Description: "Insert one or more records (JSON objects) into a table. Unknown keys are rejected; " +
			"missing required fields are rejected. Full-text and vector indexes update automatically; " +
			"vectorized fields are embedded by the server. Retried writes should pass idempotency_key: " +
			"the key and its ids are recorded durably, so a retry with the same key and the same records " +
			"returns the original ids (replayed=true, nothing re-inserted) instead of duplicating rows.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     tableProp("Table name"),
				"records": map[string]any{
					"type":        "array",
					"description": "Records to insert (JSON objects keyed by field name)",
					"items":       map[string]any{"type": "object"},
					"minItems":    1,
					"maxItems":    store.MaxRecordsPerInsert,
				},
				"idempotency_key": map[string]any{
					"type":        "string",
					"description": fmt.Sprintf("Unique client-chosen key that makes the insert safe to retry (replays return the original ids; reusing a key for different records is rejected). Printable ASCII, 1-%d bytes — maxLength and the server both count bytes, so use ASCII tokens (uuid/ulid/hash) rather than multi-byte characters", store.MaxIdempotencyKeyLen),
					"minLength":   1,
					"maxLength":   store.MaxIdempotencyKeyLen,
					// JSON Schema maxLength counts characters; the store counts
					// bytes. Restricting to printable ASCII makes the two
					// identical, so schema-valid keys are always accepted.
					"pattern": fmt.Sprintf(`^[ -~]{1,%d}$`, store.MaxIdempotencyKeyLen),
				},
			},
			"required": []string{"namespace", "table", "records"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req insertReq
			if err := decodeData(body, &req); err != nil {
				return nil, err
			}
			for i, r := range req.Records {
				if r == nil {
					return nil, badRequest("records[%d] must be an object, not null", i)
				}
			}
			key := ""
			if len(req.IdempotencyKey) > 0 {
				var k string
				if err := json.Unmarshal(req.IdempotencyKey, &k); err != nil || string(bytes.TrimSpace(req.IdempotencyKey)) == "null" {
					return nil, badRequest("idempotency_key must be a string")
				}
				if k == "" {
					return nil, badRequest("idempotency_key must not be empty — omit the field for a plain insert (an empty key would silently fall back to non-idempotent writes)")
				}
				key = k
			}
			if key != "" {
				ids, replayed, err := s.st.InsertIdempotent(ctx, normNS(req.Namespace), normTable(req.Table), req.Records, s.embedder(), key)
				if err != nil {
					return nil, wrapStoreErr(err)
				}
				inserted := len(ids)
				if replayed {
					inserted = 0
				}
				return map[string]any{"ids": ids, "inserted": inserted, "replayed": replayed}, nil
			}
			ids, err := s.st.Insert(ctx, normNS(req.Namespace), normTable(req.Table), req.Records, s.embedder())
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"ids": ids, "inserted": len(ids)}, nil
		},
	},
	"upsert_by_key": {
		Description: "Insert or update records by natural key: for each record, when an existing row has the " +
			"fields named in \"on\" equal to the record's values, that row is updated with the record's other " +
			"fields (partial update — unspecified fields keep their values); otherwise the record is inserted " +
			"and must satisfy required fields. Repeating the call converges instead of duplicating rows, so it " +
			"is the retry-safe write path when the data carries its own identity (e.g. email, url, external id). " +
			"Within a batch, later records update rows created by earlier ones with the same key. Key fields " +
			"must be scalar (string, text, number, boolean, timestamp) and present, non-null, in every record.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     tableProp("Table name"),
				"on": map[string]any{
					"type":        "array",
					"description": "Natural key: field name(s) whose values identify a row for update-vs-insert",
					"items":       fieldNameProp("Key field name"),
					"minItems":    1,
					"maxItems":    store.MaxKeyFields,
					"uniqueItems": true,
				},
				"records": map[string]any{
					"type":        "array",
					"description": "Records to insert or update (JSON objects keyed by field name)",
					"items":       map[string]any{"type": "object"},
					"minItems":    1,
					"maxItems":    store.MaxRecordsPerInsert,
				},
			},
			"required": []string{"namespace", "table", "on", "records"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req upsertReq
			if err := decodeData(body, &req); err != nil {
				return nil, err
			}
			for i, r := range req.Records {
				if r == nil {
					return nil, badRequest("records[%d] must be an object, not null", i)
				}
			}
			if len(req.On) == 0 {
				return nil, badRequest("on must name at least one key field")
			}
			ids, inserted, updated, err := s.st.UpsertByKey(ctx, normNS(req.Namespace), normTable(req.Table), req.On, req.Records, s.embedder())
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"ids": ids, "inserted": inserted, "updated": updated}, nil
		},
	},
	"query": {
		Description: "Run a read-only SQL statement (SELECT or WITH only) against one namespace. " +
			"Use table and column names from list_tables/describe_table. Bind parameters with ? and pass args. " +
			"Coercion to declared field types is by result-column label, so aliases count as their label: " +
			"a label declared boolean reads true/false, json reads decoded, vector reads a number array, " +
			"number reads integer or float. Labels that match no declared field, or that different tables " +
			"declare with different types, fall back to raw values (blobs as base64). " +
			"id and created_at are included in SELECT *; the hidden _embedding column is stripped from SELECT * — " +
			"reference _embedding in the statement (outside string literals/comments) to include it.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace to query"),
				"sql": map[string]any{
					"type":        "string",
					"description": "Read-only SQL (SELECT/WITH), at most " + strconv.Itoa(store.MaxQueryRunes) + " characters",
					"minLength":   1,
					"maxLength":   store.MaxQueryRunes,
					// Anchored to a SELECT/WITH prefix only; semicolons are
					// permitted so quoted literals like 'a;b' pass a strict
					// MCP client. The store's quote-aware guard rejects
					// genuine multi-statement input.
					"pattern": `^\s*([sS][eE][lL][eE][cC][tT]|[wW][iI][tT][hH])\b[\s\S]*$`,
				},
				"args": map[string]any{
					"type":        "array",
					"description": "Optional bind parameters for ? placeholders",
					"items": map[string]any{
						"anyOf": []any{
							map[string]any{"type": "string"},
							map[string]any{"type": "number"},
							map[string]any{"type": "boolean"},
							map[string]any{"type": "null"},
						},
					},
					"maxItems": 100,
				},
			},
			"required": []string{"namespace", "sql"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req queryReq
			if err := decodeData(body, &req); err != nil {
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
			"(e.g. \"payment\", \"'credit refund'\", \"status:ok AND retry\"). Returns matching records ordered by relevance. " +
			"Results honor declared field types (boolean -> true/false, json -> decoded value, vector -> number array) " +
			"and omit the hidden _embedding column unless include_hidden is true.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     tableProp("Table name"),
				"query": map[string]any{
					"type":        "string",
					"description": "FTS5 MATCH expression",
					"minLength":   1,
					"pattern":     `\S`,
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max results (default 10, max 200)",
					"minimum":     1,
					"maximum":     200,
				},
				"include_hidden": prop("boolean", "Also return hidden internal columns (currently _embedding) in results"),
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
			results, truncated, err := s.st.SearchFulltext(ctx, normNS(req.Namespace), normTable(req.Table), req.Query, limit(req.Limit), req.IncludeHidden)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"results": results, "truncated": truncated}, nil
		},
	},
	"search_vector": {
		Description: "Nearest-neighbor vector search. Pass text (the server embeds it) or a raw vector. " +
			"column is optional: defaults to the auto-embedding of a vectorized field, else the first vector field. " +
			"Results carry _score (cosine similarity, higher is closer), honor declared field types " +
			"(boolean -> true/false, json -> decoded value, vector -> number array), and omit the hidden " +
			"_embedding column unless include_hidden is true.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     tableProp("Table name"),
				"text": map[string]any{
					"type":        "string",
					"description": "Query text; the server embeds it (requires an embedding provider)",
					"minLength":   1,
				},
				"vector": map[string]any{
					"type":        "array",
					"description": "Raw query vector",
					"items": map[string]any{
						"type":    "number",
						"minimum": -3.4028234663852886e+38,
						"maximum": 3.4028234663852886e+38,
					},
					"minItems": 1,
				},
				"column": prop("string", "Vector column to search (optional)"),
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max results (default 10, max 200)",
					"minimum":     1,
					"maximum":     200,
				},
				"include_hidden": prop("boolean", "Also return hidden internal columns (currently _embedding) in results"),
			},
			"required": []string{"namespace", "table"},
			"oneOf": []any{
				map[string]any{"required": []string{"text"}},
				map[string]any{"required": []string{"vector"}},
			},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req vecReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			if req.Text != "" && len(req.Vector) > 0 {
				return nil, badRequest("pass either text or vector, not both")
			}
			var vec []float32
			switch {
			case req.Text != "":
				if s.emb.Identity() == "" {
					return nil, badRequest("text search requires an embedding provider with a reported identity so queries are attributable to an embedding space")
				}
				if err := s.st.ValidateVectorSearch(ctx, normNS(req.Namespace), normTable(req.Table),
					strings.ToLower(strings.TrimSpace(req.Column)), s.emb.Identity()); err != nil {
					return nil, wrapStoreErr(err)
				}
				vecs, err := s.emb.Embed(ctx, []string{req.Text})
				if err != nil {
					return nil, wrapStoreErr(err)
				}
				if len(vecs) != 1 {
					return nil, badRequest("embedding provider returned %d vectors for one query text", len(vecs))
				}
				if len(vecs[0]) == 0 {
					return nil, badRequest("embedding provider returned a zero-dimensional vector for the query text")
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
			queryIdentity := ""
			if req.Text != "" {
				queryIdentity = s.emb.Identity()
			}
			results, truncated, err := s.st.SearchVector(ctx, normNS(req.Namespace), normTable(req.Table),
				strings.ToLower(strings.TrimSpace(req.Column)), vec, queryIdentity, limit(req.Limit), req.IncludeHidden)
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
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     tableProp("Table name"),
				"filter": map[string]any{
					"type":        "string",
					"description": "SQL WHERE expression selecting rows to delete. A semicolon inside a quoted literal or comment is fine; the store rejects genuine multi-statement filters.",
					"pattern":     `\S`,
				},
				"args": map[string]any{
					"type":        "array",
					"description": "Optional bind parameters for ? placeholders",
					"items": map[string]any{
						"anyOf": []any{
							map[string]any{"type": "string"},
							map[string]any{"type": "number"},
							map[string]any{"type": "boolean"},
							map[string]any{"type": "null"},
						},
					},
				},
			},
			"required": []string{"namespace", "table", "filter"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req deleteReq
			if err := decodeData(body, &req); err != nil {
				return nil, err
			}
			deleted, err := s.st.Delete(ctx, normNS(req.Namespace), normTable(req.Table), req.Filter, req.Args)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"deleted": deleted}, nil
		},
	},
	"update": {
		Description: "Update rows matching a SQL WHERE expression (e.g. \"status = 'done'\" or \"id IN (3, 7)\") " +
			"by setting the given fields. Values are validated against the table schema; unknown fields are rejected; " +
			"a null value clears a field (required fields cannot be cleared). All matched rows get the same values. " +
			"Search indexes stay consistent: full-text rows are reindexed when an indexed field changes, and " +
			"vectorized fields are re-embedded. The filter is required; pass \"1=1\" to update every row.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     tableProp("Table name"),
				"filter": map[string]any{
					"type":        "string",
					"description": "SQL WHERE expression selecting rows to update",
					"pattern":     `\S`,
					"not":         map[string]any{"pattern": ";"},
				},
				"args": map[string]any{
					"type":        "array",
					"description": "Optional bind parameters for ? placeholders",
					"items": map[string]any{
						"anyOf": []any{
							map[string]any{"type": "string"},
							map[string]any{"type": "number"},
							map[string]any{"type": "boolean"},
							map[string]any{"type": "null"},
						},
					},
				},
				"set": map[string]any{
					"type":          "object",
					"description":   "Field values to set, keyed by field name (null clears a field)",
					"minProperties": 1,
				},
			},
			"required": []string{"namespace", "table", "filter", "set"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req updateReq
			if err := decodeData(body, &req); err != nil {
				return nil, err
			}
			updated, err := s.st.Update(ctx, normNS(req.Namespace), normTable(req.Table), req.Filter, req.Args, req.Set, s.embedder())
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"updated": updated}, nil
		},
	},
	"upsert": {
		Description: "Update rows matching a SQL WHERE expression, or insert one record when no row matches. " +
			"With matches it behaves exactly like update (all matched rows get the set values); with none it " +
			"inserts set as a new record, which must then satisfy required fields. Returns inserted=true with " +
			"the new id, or inserted=false with the updated row count.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     tableProp("Table name"),
				"filter": map[string]any{
					"type":        "string",
					"description": "SQL WHERE expression selecting the row(s) to update; insert when it matches nothing",
					"pattern":     `\S`,
					"not":         map[string]any{"pattern": ";"},
				},
				"args": map[string]any{
					"type":        "array",
					"description": "Optional bind parameters for ? placeholders",
					"items": map[string]any{
						"anyOf": []any{
							map[string]any{"type": "string"},
							map[string]any{"type": "number"},
							map[string]any{"type": "boolean"},
							map[string]any{"type": "null"},
						},
					},
				},
				"set": map[string]any{
					"type":          "object",
					"description":   "Field values to apply, keyed by field name (used as the record when inserting; null clears a field)",
					"minProperties": 1,
				},
			},
			"required": []string{"namespace", "table", "filter", "set"},
		},
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req updateReq
			if err := decodeData(body, &req); err != nil {
				return nil, err
			}
			res, err := s.st.Upsert(ctx, normNS(req.Namespace), normTable(req.Table), req.Filter, req.Args, req.Set, s.embedder())
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			out := map[string]any{"inserted": res.Inserted, "updated": res.Updated}
			if res.Inserted {
				out["id"] = res.ID
			}
			return out, nil
		},
	},
	"migrate": {
		Description: "Evolve a table schema: add_field, rename_field, drop_field, set_fulltext, set_vectorize. " +
			"Bumps the schema version and records the change. Adding fulltext rebuilds the search index; " +
			"enabling vectorize backfills embeddings for existing rows.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     tableProp("Table name"),
				"changes": map[string]any{
					"type":        "array",
					"description": "Ordered list of changes",
					"minItems":    1,
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]any{
							"op": map[string]any{
								"type":        "string",
								"description": "add_field | rename_field | drop_field | set_fulltext | set_vectorize",
								"enum":        []string{"add_field", "rename_field", "drop_field", "set_fulltext", "set_vectorize"},
							},
							"field": fieldItemSchema("Field definition for add_field"),
							"from":  fieldNameProp("Current name (rename_field)"),
							"to":    fieldNameProp("New name (rename_field)"),
							"name":  fieldNameProp("Field name (drop_field, set_fulltext, set_vectorize)"),
							"value": prop("boolean", "Flag value (set_fulltext, set_vectorize)"),
						},
						"required": []string{"op"},
						"allOf": []any{
							map[string]any{
								"if":   map[string]any{"properties": map[string]any{"op": map[string]any{"const": "add_field"}}},
								"then": map[string]any{"required": []string{"field"}},
							},
							map[string]any{
								"if":   map[string]any{"properties": map[string]any{"op": map[string]any{"const": "rename_field"}}},
								"then": map[string]any{"required": []string{"from", "to"}},
							},
							map[string]any{
								"if":   map[string]any{"properties": map[string]any{"op": map[string]any{"const": "drop_field"}}},
								"then": map[string]any{"required": []string{"name"}},
							},
							map[string]any{
								"if":   map[string]any{"properties": map[string]any{"op": map[string]any{"const": "set_fulltext"}}},
								"then": map[string]any{"required": []string{"name", "value"}},
							},
							map[string]any{
								"if":   map[string]any{"properties": map[string]any{"op": map[string]any{"const": "set_vectorize"}}},
								"then": map[string]any{"required": []string{"name", "value"}},
							},
							map[string]any{
								"if": map[string]any{
									"properties": map[string]any{
										"op":    map[string]any{"const": "set_fulltext"},
										"value": map[string]any{"const": true},
									},
									"required": []string{"op", "value"},
								},
								"then": map[string]any{
									"properties": map[string]any{
										"name": map[string]any{"not": map[string]any{"const": "rank"}},
									},
								},
							},
						},
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
			var shadow struct {
				Namespace string           `json:"namespace"`
				Table     string           `json:"table"`
				Changes   []map[string]any `json:"changes"`
			}
			if err := decodeData(body, &shadow); err != nil {
				return nil, err
			}
			for i, ch := range shadow.Changes {
				op, _ := ch["op"].(string)
				if op == "set_fulltext" || op == "set_vectorize" {
					if _, ok := ch["value"]; !ok {
						return nil, badRequest("changes[%d]: %s requires an explicit value (true or false); an omitted value would silently disable the feature and clear its index", i, op)
					}
				}
			}
			sc, err := s.st.Migrate(ctx, normNS(req.Namespace), normTable(req.Table), req.Changes, s.embedder())
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"table": sc}, nil
		},
	},
}

type insertReq struct {
	Namespace      string           `json:"namespace"`
	Table          string           `json:"table"`
	Records        []map[string]any `json:"records"`
	IdempotencyKey json.RawMessage  `json:"idempotency_key"`
}

type upsertReq struct {
	Namespace string           `json:"namespace"`
	Table     string           `json:"table"`
	On        []string         `json:"on"`
	Records   []map[string]any `json:"records"`
}

type queryReq struct {
	Namespace string `json:"namespace"`
	SQL       string `json:"sql"`
	Args      []any  `json:"args"`
}

type ftsReq struct {
	Namespace     string `json:"namespace"`
	Table         string `json:"table"`
	Query         string `json:"query"`
	Limit         int    `json:"limit"`
	IncludeHidden bool   `json:"include_hidden"`
}

type vecReq struct {
	Namespace     string    `json:"namespace"`
	Table         string    `json:"table"`
	Column        string    `json:"column"`
	Text          string    `json:"text"`
	Vector        []float64 `json:"vector"`
	Limit         int       `json:"limit"`
	IncludeHidden bool      `json:"include_hidden"`
}

type deleteReq struct {
	Namespace string `json:"namespace"`
	Table     string `json:"table"`
	Filter    string `json:"filter"`
	Args      []any  `json:"args"`
}

type updateReq struct {
	Namespace string         `json:"namespace"`
	Table     string         `json:"table"`
	Filter    string         `json:"filter"`
	Args      []any          `json:"args"`
	Set       map[string]any `json:"set"`
}

type migrateReq struct {
	Namespace string          `json:"namespace"`
	Table     string          `json:"table"`
	Changes   []schema.Change `json:"changes"`
}
