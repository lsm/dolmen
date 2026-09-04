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

func outSchema(props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func fieldOutSchema(desc string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": desc,
		"properties": map[string]any{
			"name": prop("string", "Field name"),
			"type": map[string]any{
				"type":        "string",
				"description": "Field type",
				"enum": []schema.FieldType{
					schema.String, schema.Text, schema.Number, schema.Boolean,
					schema.Timestamp, schema.JSON, schema.Vector,
				},
			},
			"fulltext":  prop("boolean", "Present and true when the field is full-text indexed"),
			"vectorize": prop("boolean", "Present and true when the server embeds the field automatically"),
			"dim":       prop("integer", "Vector dimension (present on vector fields)"),
			"required":  prop("boolean", "Present and true when inserts must provide the field"),
		},
		"required":             []string{"name", "type"},
		"additionalProperties": false,
	}
}

func tableOutSchema(desc string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": desc,
		"properties": map[string]any{
			"namespace":   prop("string", "Namespace of the table"),
			"name":        prop("string", "Table name"),
			"version":     prop("integer", "Schema version (starts at 1, bumps on migrate)"),
			"fields":      map[string]any{"type": "array", "description": "Field definitions", "items": fieldOutSchema("Field definition")},
			"embed_space": prop("string", "Embedding space of the vectorize field (present when set)"),
			"embed_dim":   prop("integer", "Dimension of the server-side embedding (present when set)"),
		},
		"required":             []string{"namespace", "name", "version", "fields"},
		"additionalProperties": false,
	}
}

// changeOutSchema describes a recorded migration change (history entries and
// plans), mirroring the migrate input shape including add_field defaults and
// the explicit value set_* changes always record.
func changeOutSchema(desc string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": desc,
		"properties": map[string]any{
			"op": prop("string", "add_field | rename_field | drop_field | set_fulltext | set_vectorize"),
			"field": map[string]any{
				"type":                 "object",
				"description":          "Field definition (add_field)",
				"additionalProperties": false,
				"properties": map[string]any{
					"name":      prop("string", "Field name"),
					"type":      prop("string", "Field type"),
					"fulltext":  prop("boolean", "Present and true when full-text indexed"),
					"vectorize": prop("boolean", "Present and true when server-embedded"),
					"dim":       prop("integer", "Vector dimension (vector fields)"),
					"required":  prop("boolean", "Present and true when inserts must provide the field"),
				},
				"required": []string{"name"},
			},
			"from":    prop("string", "Current name (rename_field)"),
			"to":      prop("string", "New name (rename_field)"),
			"name":    prop("string", "Field name (drop_field, set_fulltext, set_vectorize)"),
			"value":   prop("boolean", "Flag value (set_fulltext, set_vectorize)"),
			"default": map[string]any{"description": "Backfill value for existing rows (add_field), exactly as applied"},
		},
		"required":             []string{"op"},
		"additionalProperties": false,
	}
}

func planOutSchema(desc string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": desc,
		"properties": map[string]any{
			"dry_run":               prop("boolean", "Always true for a plan (nothing was applied)"),
			"from_version":          prop("integer", "Schema version the changes were planned against"),
			"to_version":            prop("integer", "Version the table would have after applying"),
			"table":                 tableOutSchema("Prospective schema after the changes"),
			"operations":            map[string]any{"type": "array", "description": "Human-readable operations, in order", "items": map[string]any{"type": "string"}},
			"destructive":           map[string]any{"type": "array", "description": "Destructive changes with their consequence (present when any)", "items": map[string]any{"type": "string"}},
			"backfill_rows":         prop("integer", "Existing rows that receive an added field's default"),
			"rebuild_fulltext":      prop("boolean", "Whether the FTS index is rebuilt"),
			"fulltext_reindex_rows": prop("integer", "Rows the rebuilt full-text index would hold"),
			"clears_embeddings":     prop("boolean", "Whether existing embeddings are cleared"),
			"embed_rows":            prop("integer", "Rows applying would embed (provider calls)"),
		},
		"required":             []string{"dry_run", "from_version", "to_version", "table", "operations", "backfill_rows", "rebuild_fulltext", "fulltext_reindex_rows", "clears_embeddings", "embed_rows"},
		"additionalProperties": false,
	}
}

var Ops = map[string]OpDef{
	"list_tables": {
		Description: "List tables in a namespace.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{"namespace": nsProp("Namespace to list tables in")},
			"required":             []string{"namespace"},
		},
		OutputSchema: outSchema(map[string]any{
			"tables": map[string]any{
				"type":        "array",
				"description": "Table names in the namespace",
				"items":       map[string]any{"type": "string"},
			},
		}, "tables"),
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
	"list_namespaces": {
		Description: "List the namespaces on this server (one isolated SQLite file per namespace). " +
			"Use it to see which namespaces already exist before creating or reusing one.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{},
		},
		OutputSchema: outSchema(map[string]any{
			"namespaces": map[string]any{
				"type":        "array",
				"description": "Namespace names under the data directory, sorted",
				"items":       map[string]any{"type": "string"},
			},
		}, "namespaces"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req struct{}
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			nss, err := s.st.ListNamespaces()
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			if nss == nil {
				nss = []string{}
			}
			return map[string]any{"namespaces": nss}, nil
		},
	},
	"create_namespace": {
		Description: "Create an empty namespace. Namespaces are also created implicitly on first use, " +
			"so this is only needed to reserve a name up front or to fail loudly when the name is taken. " +
			"Creates no tables — follow with create_table.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{"namespace": nsProp("Namespace to create")},
			"required":             []string{"namespace"},
		},
		OutputSchema: outSchema(map[string]any{
			"namespace": prop("string", "The created namespace name"),
		}, "namespace"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req nsReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			ns := normNS(req.Namespace)
			if err := s.st.CreateNamespace(ns); err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"namespace": ns}, nil
		},
	},
	"drop_namespace": {
		Description: "Drop a namespace and every table in it, deleting its SQLite file and WAL sidecars. " +
			"Irreversible. confirm must repeat the exact namespace name — a guard against dropping the wrong one. " +
			"In-flight requests on the namespace finish first (or fail); any later use of the same name recreates " +
			"the namespace empty. The server closes its own connections before deleting, but other processes " +
			"holding the file open (a second dolmen, a backup tool) are not detected — coordinate drops within one server.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace to drop"),
				"confirm": map[string]any{
					"type":        "string",
					"description": "Safety guard: repeat the exact namespace name here to confirm the irreversible drop",
					"minLength":   1,
				},
			},
			"required": []string{"namespace", "confirm"},
		},
		OutputSchema: outSchema(map[string]any{
			"dropped": prop("string", "The dropped namespace name"),
		}, "dropped"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req dropNamespaceReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			ns := normNS(req.Namespace)
			if normNS(req.Confirm) != ns {
				return nil, badRequest("confirm must repeat the exact namespace name %q to drop it", ns)
			}
			if err := s.st.DropNamespace(ns); err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"dropped": ns}, nil
		},
	},
	"drop_table": {
		Description: "Drop a table: its rows, its full-text index, its schema and migration history, and its " +
			"idempotency keys. Irreversible. confirm must repeat the exact table name — a guard against dropping " +
			"the wrong one. A table recreated under the same name starts fresh (version 1, empty, no history).",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     existingTableProp("Table name"),
				"confirm": map[string]any{
					"type":        "string",
					"description": "Safety guard: repeat the exact table name here to confirm the irreversible drop",
					"minLength":   1,
				},
			},
			"required": []string{"namespace", "table", "confirm"},
		},
		OutputSchema: outSchema(map[string]any{
			"dropped": prop("string", "The dropped table name"),
		}, "dropped"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req dropTableReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			table := normTable(req.Table)
			if normTable(req.Confirm) != table {
				return nil, badRequest("confirm must repeat the exact table name %q to drop it", table)
			}
			if err := s.st.DropTable(ctx, normNS(req.Namespace), table); err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"dropped": table}, nil
		},
	},
	"describe_table": {
		Description: "Get the schema, version, and row count of a table.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     existingTableProp("Table name"),
			},
			"required": []string{"namespace", "table"},
		},
		OutputSchema: outSchema(map[string]any{
			"table":     tableOutSchema("Table schema"),
			"row_count": prop("integer", "Number of rows currently in the table"),
		}, "table", "row_count"),
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
				"table":     tableProp("Table name (lowercase [a-z0-9_]; no sqlite_/pragma_ prefix, __fts, or dbstat; not a SQLite/SQL keyword or reserved name)"),
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
		OutputSchema: outSchema(map[string]any{
			"table": tableOutSchema("Schema of the created table"),
		}, "table"),
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
		OutputSchema: outSchema(map[string]any{
			"fields": map[string]any{
				"type":        "array",
				"description": "Proposed field definitions",
				"items":       fieldOutSchema("Proposed field definition"),
			},
			"warnings": map[string]any{
				"type":        "array",
				"description": "Notes about sanitized or merged keys",
				"items":       map[string]any{"type": "string"},
			},
			"provenance": map[string]any{
				"type":                 "object",
				"description":          "Map from inferred field name to the original key(s) that produced it",
				"additionalProperties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		}, "fields", "warnings", "provenance"),
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
			inf := schema.InferSchema(req.Samples)
			if inf.Fields == nil {
				inf.Fields = []schema.Field{}
			}
			if inf.Warnings == nil {
				inf.Warnings = []string{}
			}
			if inf.Provenance == nil {
				inf.Provenance = map[string][]string{}
			}
			return map[string]any{
				"fields":     inf.Fields,
				"warnings":   inf.Warnings,
				"provenance": inf.Provenance,
			}, nil
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
				"table":     existingTableProp("Table name"),
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
		OutputSchema: outSchema(map[string]any{
			"ids": map[string]any{
				"type":        "array",
				"description": "Row ids assigned to the inserted records, in order",
				"items":       map[string]any{"type": "integer"},
			},
			"inserted": prop("integer", "Number of records inserted"),
			"replayed": prop("boolean", "True when an idempotency_key replayed a previous insert (original ids returned, nothing re-inserted); present only for idempotent inserts"),
		}, "ids", "inserted"),
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
				"table":     existingTableProp("Table name"),
				"on": map[string]any{
					"type":        "array",
					"description": "Natural key: field name(s) whose values identify a row for update-vs-insert",
					"items":       existingFieldNameProp("Key field name"),
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
		OutputSchema: outSchema(map[string]any{
			"ids": map[string]any{
				"type":        "array",
				"description": "Row ids after insert-or-update, in record order",
				"items":       map[string]any{"type": "integer"},
			},
			"inserted": prop("integer", "Number of records inserted"),
			"updated":  prop("integer", "Number of existing rows updated"),
		}, "ids", "inserted", "updated"),
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
			"reference _embedding in the statement (outside string literals/comments) to include it. " +
			"Do not put LIMIT or OFFSET in the SQL; use the offset and limit parameters. " +
			"For stable pagination, include an explicit ORDER BY clause.",
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
				"offset": map[string]any{
					"type":        "integer",
					"description": "Rows to skip (default 0)",
					"minimum":     0,
					"maximum":     1000000000,
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max rows to return (default 1000, max 1000)",
					"minimum":     1,
					"maximum":     1000,
				},
			},
			"required": []string{"namespace", "sql"},
		},
		OutputSchema: outSchema(map[string]any{
			"rows": map[string]any{
				"type":        "array",
				"description": "Rows keyed by column name; declared fields honor their types (vector columns read as number arrays, json fields decoded), undeclared labels fall back to raw values",
				"items":       map[string]any{"type": "object", "description": "Row keyed by column name"},
			},
			"row_count": prop("integer", "Number of rows returned"),
			"truncated": prop("boolean", "True when more results are available beyond the returned page (because the limit was reached or the response budget was hit)"),
		}, "rows", "row_count", "truncated"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req queryReq
			if err := decodeData(body, &req); err != nil {
				return nil, err
			}
			rows, truncated, err := s.st.Query(ctx, normNS(req.Namespace), req.SQL, req.Args, req.Offset, req.Limit)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"rows": rows, "row_count": len(rows), "truncated": truncated}, nil
		},
	},
	"search_fulltext": {
		Description: "Full-text search over fields marked fulltext, using SQLite FTS5 MATCH syntax " +
			"(e.g. \"payment\", \"credit refund\", \"status:ok AND retry\"). Returns matching records ordered by relevance (stable rowid tie-breaking). " +
			"Results honor declared field types (boolean -> true/false, json -> decoded value, vector -> number array) " +
			"and omit the hidden _embedding column unless include_hidden is true.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     existingTableProp("Table name"),
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
				"offset": map[string]any{
					"type":        "integer",
					"description": "Results to skip (default 0)",
					"minimum":     0,
					"maximum":     1000000000,
				},
				"include_hidden": prop("boolean", "Also return hidden internal columns (currently _embedding) in results"),
			},
			"required": []string{"namespace", "table", "query"},
		},
		OutputSchema: outSchema(map[string]any{
			"results": map[string]any{
				"type":        "array",
				"description": "Matching records ordered by relevance (id, created_at, and table fields)",
				"items":       map[string]any{"type": "object", "description": "Matching record"},
			},
			"truncated": prop("boolean", "True when more results are available beyond the returned page (because the limit was reached or the response budget was hit)"),
		}, "results", "truncated"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req ftsReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			if req.Query == "" {
				return nil, badRequest("query must not be empty")
			}
			results, truncated, err := s.st.SearchFulltext(ctx, normNS(req.Namespace), normTable(req.Table), req.Query, req.Offset, limit(req.Limit), req.IncludeHidden)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"results": results, "truncated": truncated}, nil
		},
	},
	"search_vector": {
		Description: "Nearest-neighbor vector search. Pass text (the server embeds it) or a raw vector. " +
			"column is optional for raw vectors: defaults to the auto-embedding of a vectorized field, else the first vector field. " +
			"Text queries always target the server-managed vectorize (_embedding) space and are rejected for caller-provided " +
			"vector columns — their embedding space is unknown, so cosine against a freshly embedded query would be meaningless; " +
			"search those with a raw vector from the same embedding space that produced the stored vectors. " +
			"Optional filter and args restrict rows with a SQL WHERE expression (like delete's filter) " +
			"before scoring; optional min_score drops lower-similarity results before the ranking/limit. " +
			"Results carry _score (cosine similarity, higher is closer), ordered by score with stable id tie-breaking, honor declared field types " +
			"(boolean -> true/false, json -> decoded value, vector -> number array), and omit the hidden " +
			"_embedding column unless include_hidden is true. skipped_vectors counts rows whose stored vector " +
			"was corrupt or dimension-mismatched and could not be scored; a nonzero count means those rows are missing from results.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     existingTableProp("Table name"),
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
				"column": prop("string", "Vector column to search (raw-vector queries only; text queries always search the vectorize _embedding space)"),
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max results (default 10, max 200)",
					"minimum":     1,
					"maximum":     200,
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Results to skip (default 0)",
					"minimum":     0,
					"maximum":     1000000000,
				},
				"include_hidden": prop("boolean", "Also return hidden internal columns (currently _embedding) in results"),
				"filter": map[string]any{
					"type":        "string",
					"description": "Optional SQL WHERE expression filtering rows before vector scoring (like delete's filter)",
					"pattern":     `\S`,
					"not":         map[string]any{"pattern": ";"},
				},
				"args": map[string]any{
					"type":        "array",
					"description": "Optional bind parameters for ? placeholders in filter",
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
				"min_score": map[string]any{
					"type":        "number",
					"description": "Optional minimum cosine-similarity score (inclusive); results below this are dropped before ranking and limit",
				},
			},
			"required": []string{"namespace", "table"},
			"oneOf": []any{
				map[string]any{"required": []string{"text"}},
				map[string]any{"required": []string{"vector"}},
			},
		},
		OutputSchema: outSchema(map[string]any{
			"results": map[string]any{
				"type":        "array",
				"description": "Nearest records ordered by similarity (higher _score is closer)",
				"items": map[string]any{
					"type":        "object",
					"description": "Nearest record with _score; the searched vector column carries decoded floats",
					"properties": map[string]any{
						"_score": map[string]any{
							"type":        "number",
							"description": "Cosine similarity to the query vector (higher is closer)",
						},
					},
				},
			},
			"truncated":       prop("boolean", "True when more results are available beyond the returned page (because the limit was reached or the response budget was hit)"),
			"skipped_vectors": prop("integer", "Rows whose stored vector was corrupt or dimension-mismatched and could not be scored; nonzero means those rows are missing from results"),
		}, "results", "truncated", "skipped_vectors"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req vecReq
			if err := decodeAllowNullArgs(body, &req); err != nil {
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
			res, err := s.st.SearchVector(ctx, normNS(req.Namespace), normTable(req.Table),
				strings.ToLower(strings.TrimSpace(req.Column)), vec, queryIdentity, req.Offset, limit(req.Limit), req.IncludeHidden,
				req.Filter, req.Args, req.MinScore)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"results": res.Rows, "truncated": res.Truncated, "skipped_vectors": res.Skipped}, nil
		},
	},
	"delete": {
		Description: "Delete rows matching a SQL WHERE expression (e.g. \"status = 'done'\" or \"id IN (3, 7)\"). " +
			"Rows are also removed from search indexes. Use dry_run to preview the matched count, limit to set a safe threshold, " +
			"and confirm: true to delete beyond the threshold. Without an explicit limit, deletes beyond " + fmt.Sprintf("%d", store.DefaultDeleteLimit) + " matching rows require confirm: true.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     existingTableProp("Table name"),
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
				"dry_run": prop("boolean", "If true, only count matching rows and do not delete; returns matched with deleted: 0"),
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of matching rows that can be deleted without confirmation; if more rows match, confirm: true is required",
					"minimum":     1,
				},
				"confirm": prop("boolean", "If true, allow the delete to proceed when the number of matching rows exceeds the limit (or the default limit if no limit is set)"),
			},
			"required": []string{"namespace", "table", "filter"},
		},
		OutputSchema: outSchema(map[string]any{
			"matched": prop("integer", "Number of rows matching the filter"),
			"deleted": prop("integer", "Number of rows actually deleted (0 when dry_run is true)"),
		}, "matched", "deleted"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req deleteReq
			if err := decodeData(body, &req); err != nil {
				return nil, err
			}
			dryRun, err := parseOptBool(req.DryRun, "dry_run")
			if err != nil {
				return nil, err
			}
			limit, err := parseOptPosInt(req.Limit, "limit")
			if err != nil {
				return nil, err
			}
			confirm, err := parseOptBool(req.Confirm, "confirm")
			if err != nil {
				return nil, err
			}
			res, err := s.st.Delete(ctx, normNS(req.Namespace), normTable(req.Table), req.Filter, req.Args, store.DeleteOptions{
				DryRun:  dryRun,
				Limit:   limit,
				Confirm: confirm,
			})
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"matched": res.Matched, "deleted": res.Deleted}, nil
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
				"table":     existingTableProp("Table name"),
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
		OutputSchema: outSchema(map[string]any{
			"updated": prop("integer", "Number of rows updated"),
		}, "updated"),
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
				"table":     existingTableProp("Table name"),
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
		OutputSchema: outSchema(map[string]any{
			"inserted": prop("boolean", "True when no row matched and a new record was inserted"),
			"updated":  prop("integer", "Number of rows updated (0 when a record was inserted)"),
			"id":       prop("integer", "Row id of the inserted record (present only when inserted is true)"),
		}, "inserted", "updated"),
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
			"enabling vectorize backfills embeddings for existing rows. add_field accepts a default that is " +
			"coerced to the field's type and backfilled into existing rows; it is required for adding a " +
			"required field to a populated table (the column then carries NOT NULL DEFAULT — dolmen inserts " +
			"must still supply the field). For optional fields the default is a one-time backfill: later " +
			"inserts omitting the field store NULL. Pass expected_version (from describe_table) to assert " +
			"the schema the changes were planned against: a mismatch fails with a conflict instead of " +
			"running a stale plan (required for the destructive rename_field and drop_field). Pass " +
			"dry_run=true to validate and preview — prospective schema and version, destructive changes, " +
			"backfill rows, index rebuild, and embedding workload — with nothing applied and no provider " +
			"calls.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     existingTableProp("Table name"),
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
							"from":  existingFieldNameProp("Current name (rename_field)"),
							"to":    fieldNameProp("New name (rename_field)"),
							"name":  existingFieldNameProp("Field name (drop_field, set_fulltext, set_vectorize)"),
							"value": prop("boolean", "Flag value (set_fulltext, set_vectorize)"),
							"default": map[string]any{
								"description": "Backfill value for existing rows (add_field only); coerced to the field's type — a string for string/text/timestamp/json, number, boolean, or a number array of the field's dim for vector",
							},
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
										"op": map[string]any{"not": map[string]any{"const": "add_field"}},
									},
									"required": []string{"op"},
								},
								"then": map[string]any{"not": map[string]any{"required": []string{"default"}}},
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
				"expected_version": map[string]any{
					"type":        "integer",
					"description": "Schema version the changes were planned against (from describe_table); the migration aborts with a conflict if the table has moved past it. Required for rename_field and drop_field.",
					"minimum":     1,
				},
				"dry_run": prop("boolean", "Validate and preview the migration without applying anything (no writes, no embedding calls)"),
			},
			"required": []string{"namespace", "table", "changes"},
		},
		OutputSchema: outSchema(map[string]any{
			"table":   tableOutSchema("Schema of the migrated table (version bumped); for dry_run, the prospective schema"),
			"dry_run": prop("boolean", "True when this was a validation-only preview (nothing applied)"),
			"plan":    planOutSchema("Migration preview (present when dry_run)"),
		}, "table"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req migrateReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			var shadow struct {
				Namespace       string           `json:"namespace"`
				Table           string           `json:"table"`
				Changes         []map[string]any `json:"changes"`
				ExpectedVersion *int             `json:"expected_version"`
				DryRun          bool             `json:"dry_run"`
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
			ver := 0
			if req.ExpectedVersion != nil {
				if *req.ExpectedVersion < 1 {
					return nil, badRequest("expected_version must be >= 1")
				}
				ver = *req.ExpectedVersion
			}
			if req.DryRun {
				plan, err := s.st.PlanMigration(ctx, normNS(req.Namespace), normTable(req.Table), req.Changes, s.embedder(), ver)
				if err != nil {
					return nil, wrapStoreErr(err)
				}
				return map[string]any{"table": plan.Table, "dry_run": true, "plan": plan}, nil
			}
			sc, err := s.st.Migrate(ctx, normNS(req.Namespace), normTable(req.Table), req.Changes, s.embedder(), ver)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"table": sc}, nil
		},
	},
	"list_migrations": {
		Description: "List a table's migration history, newest first: version transitions with the exact " +
			"recorded changes and timestamps. Read-only audit of schema evolution; the newest entry's " +
			"to_version is the current schema version (creating the table is version 1 and predates the log).",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     existingTableProp("Table name"),
			},
			"required": []string{"namespace", "table"},
		},
		OutputSchema: outSchema(map[string]any{
			"migrations": map[string]any{
				"type":        "array",
				"description": "Recorded migrations, newest first",
				"items": map[string]any{
					"type":        "object",
					"description": "One recorded schema transition",
					"properties": map[string]any{
						"id":           prop("integer", "History entry id (monotonic)"),
						"from_version": prop("integer", "Schema version before the migration"),
						"to_version":   prop("integer", "Schema version after the migration"),
						"changes": map[string]any{
							"type":        "array",
							"description": "Recorded change list, replayable through migrate (add_field defaults and explicit set_* values included)",
							"items":       changeOutSchema("Recorded change"),
						},
						"at": prop("string", "When the migration committed (RFC 3339)"),
					},
					"required":             []string{"id", "from_version", "to_version", "changes", "at"},
					"additionalProperties": false,
				},
			},
		}, "migrations"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req tableReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			ms, err := s.st.ListMigrations(ctx, normNS(req.Namespace), normTable(req.Table))
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"migrations": ms}, nil
		},
	},
}

type insertReq struct {
	Namespace      string           `json:"namespace"`
	Table          string           `json:"table"`
	Records        []map[string]any `json:"records"`
	IdempotencyKey json.RawMessage  `json:"idempotency_key"`
}

type dropNamespaceReq struct {
	Namespace string `json:"namespace"`
	Confirm   string `json:"confirm"`
}

type dropTableReq struct {
	Namespace string `json:"namespace"`
	Table     string `json:"table"`
	Confirm   string `json:"confirm"`
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
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}

type ftsReq struct {
	Namespace     string `json:"namespace"`
	Table         string `json:"table"`
	Query         string `json:"query"`
	Offset        int    `json:"offset"`
	Limit         int    `json:"limit"`
	IncludeHidden bool   `json:"include_hidden"`
}

type vecReq struct {
	Namespace     string    `json:"namespace"`
	Table         string    `json:"table"`
	Column        string    `json:"column"`
	Text          string    `json:"text"`
	Vector        []float64 `json:"vector"`
	Offset        int       `json:"offset"`
	Limit         int       `json:"limit"`
	IncludeHidden bool      `json:"include_hidden"`
	Filter        string    `json:"filter"`
	Args          []any     `json:"args"`
	MinScore      *float64  `json:"min_score"`
}

type deleteReq struct {
	Namespace string          `json:"namespace"`
	Table     string          `json:"table"`
	Filter    string          `json:"filter"`
	Args      []any           `json:"args"`
	DryRun    json.RawMessage `json:"dry_run,omitempty"`
	Limit     json.RawMessage `json:"limit,omitempty"`
	Confirm   json.RawMessage `json:"confirm,omitempty"`
}

func parseOptBool(raw json.RawMessage, what string) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return false, badRequest("%s must be a boolean", what)
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, badRequest("%s must be a boolean", what)
	}
	return v, nil
}

func parseOptPosInt(raw json.RawMessage, what string) (int, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return 0, badRequest("%s must be an integer", what)
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, badRequest("%s must be an integer", what)
	}
	if v < 1 {
		return 0, badRequest("%s must be at least 1", what)
	}
	return v, nil
}

type updateReq struct {
	Namespace string         `json:"namespace"`
	Table     string         `json:"table"`
	Filter    string         `json:"filter"`
	Args      []any          `json:"args"`
	Set       map[string]any `json:"set"`
}

type migrateReq struct {
	Namespace       string          `json:"namespace"`
	Table           string          `json:"table"`
	Changes         []schema.Change `json:"changes"`
	ExpectedVersion *int            `json:"expected_version"`
	DryRun          bool            `json:"dry_run"`
}
