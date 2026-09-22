package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lsm/dolmen/internal/ops"
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
			"enum": map[string]any{
				"type":        "array",
				"description": "Allowed values for this string field (present when the field has an enum constraint); writes carrying any other value are rejected",
				"items":       map[string]any{"type": "string"},
			},
			"default": map[string]any{"description": "Value stored when an insert omits the field; exactly as declared (present when set) — \"now()\" on a timestamp field stamps the server's current time at each write"},
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

func changeOutSchema(desc string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": desc,
		"properties": map[string]any{
			"op": prop("string", "add_field | rename_field | drop_field | set_fulltext | set_vectorize | set_enum"),
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
					"enum": map[string]any{
						"type":        "array",
						"description": "Allowed values (present on enum-constrained string fields)",
						"items":       map[string]any{"type": "string"},
					},
				},
				"required": []string{"name"},
			},
			"from":  prop("string", "Current name (rename_field)"),
			"to":    prop("string", "New name (rename_field)"),
			"name":  prop("string", "Field name (drop_field, set_fulltext, set_vectorize, set_enum)"),
			"value": prop("boolean", "Flag value (set_fulltext, set_vectorize)"),
			"enum": map[string]any{
				"type":        "array",
				"description": "The field's complete new vocabulary (set_enum); an empty array removes the constraint",
				"items":       map[string]any{"type": "string", "minLength": 1},
				"uniqueItems": true,
			},
			"default": map[string]any{"description": "Backfill value for existing rows (add_field), exactly as applied"},
		},
		"required":             []string{"op"},
		"additionalProperties": false,
	}
}

func migrateOutSchema(table, planTable map[string]any) map[string]any {
	return outSchema(map[string]any{
		"table":   table,
		"dry_run": prop("boolean", "True when this was a validation-only preview (nothing applied)"),
		"plan":    planOutSchema("Migration preview (present when dry_run)", planTable),
	}, "table")
}

func planOutSchema(desc string, table map[string]any) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": desc,
		"properties": map[string]any{
			"dry_run":               prop("boolean", "Always true for a plan (nothing was applied)"),
			"from_version":          prop("integer", "Schema version the changes were planned against"),
			"to_version":            prop("integer", "Version the table would have after applying"),
			"table":                 table,
			"operations":            map[string]any{"type": "array", "description": "Human-readable operations, in order", "items": map[string]any{"type": "string"}},
			"destructive":           map[string]any{"type": "array", "description": "Destructive changes with their consequence (present when any)", "items": map[string]any{"type": "string"}},
			"backfill_rows":         prop("integer", "Existing rows that receive an added field's default"),
			"expected_incarnation":  prop("string", "Opaque token naming the table this plan was made against; pass it to apply as expected_incarnation and the migration is refused if the table moved on or was recreated"),
			"rebuild_fulltext":      prop("boolean", "Whether the FTS index is rebuilt"),
			"fulltext_reindex_rows": prop("integer", "Rows the rebuilt full-text index would hold"),
			"clears_embeddings":     prop("boolean", "Whether existing embeddings are cleared"),
			"embed_rows":            prop("integer", "Rows applying would embed (provider calls)"),
		},
		"required":             []string{"dry_run", "from_version", "to_version", "table", "operations", "backfill_rows", "rebuild_fulltext", "fulltext_reindex_rows", "clears_embeddings", "embed_rows"},
		"additionalProperties": false,
	}
}

func writeOutSchema(withUpdated, withReplayed bool) map[string]any {
	props := map[string]any{
		"ids": map[string]any{
			"type":        "array",
			"description": "Row ids the write inserted or updated, in order (a replayed insert returns the original ids)",
			"items":       map[string]any{"type": "integer"},
		},
		"inserted": prop("integer", "Number of rows inserted (0 when an idempotency key replayed a previous insert)"),
	}
	required := []string{"ids", "inserted"}
	if withUpdated {
		props["updated"] = prop("integer", "Number of existing rows updated")
		required = append(required, "updated")
	}
	if withReplayed {
		props["replayed"] = prop("boolean", "True when an idempotency_key replayed a previous insert (original ids returned, nothing re-inserted)")
	}
	return outSchema(props, required...)
}

func changesPageOutSchema() map[string]any {
	return outSchema(map[string]any{
		"changes": map[string]any{
			"type":        "array",
			"description": "Changes committed after the cursor, in commit order (§0.6 serial observability)",
			"items": map[string]any{
				"type":        "object",
				"description": "One committed change; carries identity only, never a row snapshot",
				"properties": map[string]any{
					"cursor": prop("string", "Opaque token at this change's position; persist it to resume exactly after this change"),
					"table":  prop("string", "Table the change committed in"),
					"row_id": prop("integer", "Row the change touched (re-read its current content by id)"),
					"kind": map[string]any{
						"type":        "string",
						"description": "Kind of change",
						"enum":        []store.ChangeKind{store.ChangeInsert, store.ChangeUpdate, store.ChangeDelete},
					},
				},
				"required":             []string{"cursor", "table", "row_id", "kind"},
				"additionalProperties": false,
			},
		},
		"next_cursor": prop("string", "Opaque token at the page's end; pass it as cursor to continue gap-free (an empty page still carries it)"),
	}, "changes", "next_cursor")
}

var migrateChangeKeys = map[string][]string{
	schema.OpAddField:     {"op", "field", "default"},
	schema.OpRenameField:  {"op", "from", "to"},
	schema.OpDropField:    {"op", "name"},
	schema.OpSetFulltext:  {"op", "name", "value"},
	schema.OpSetVectorize: {"op", "name", "value"},
	schema.OpSetEnum:      {"op", "name", "enum"},
}

var migrateAuthChangeKeys = map[string][]string{
	schema.OpSetRowAccess: {"op", "value"},
}

var migrateFieldDefKeys = map[string]bool{
	"name":      true,
	"type":      true,
	"dim":       true,
	"fulltext":  true,
	"vectorize": true,
	"required":  true,
}

func validateMigrateChanges(changes []map[string]any, authOn bool) error {
	valid := "add_field, rename_field, drop_field, set_fulltext, set_vectorize, set_enum"
	if authOn {
		valid += ", set_row_access"
	}
	for i, ch := range changes {
		rawOp, present := ch["op"]
		op, isString := rawOp.(string)
		if !present || !isString || op == "" {
			return badRequest("changes[%d]: op must be a non-empty string naming the change (%s)", i, valid)
		}
		keys, known := migrateChangeKeys[op]
		if !known && authOn {
			keys, known = migrateAuthChangeKeys[op]
		}
		if !known {
			return badRequest("changes[%d]: unknown migration op %q (valid: %s)", i, op, valid)
		}
		var unknown []string
		for k := range ch {
			if !slices.Contains(keys, k) {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			hint := ""
			for _, k := range unknown {
				if migrateFieldDefKeys[k] {
					hint = `; field definitions belong inside the "field" object for add_field`
					break
				}
			}
			return badRequest(`changes[%d]: unknown key %q on %s (valid keys: %s)%s`,
				i, unknown[0], op, strings.Join(keys, ", "), hint)
		}
		if op == schema.OpSetFulltext || op == schema.OpSetVectorize {
			if _, ok := ch["value"]; !ok {
				return badRequest("changes[%d]: %s requires an explicit value (true or false); an omitted value would silently disable the feature and clear its index", i, op)
			}
		}
		if op == schema.OpSetEnum {
			if _, ok := ch["enum"]; !ok {
				return badRequest("changes[%d]: set_enum requires an explicit enum array (the field's complete new vocabulary; an omitted array is ambiguous — pass an empty array to remove the constraint)", i)
			}
		}
	}
	return nil
}

const (
	defaultWaitForTimeoutMS = 30000
	maxWaitForTimeoutMS     = 60000
	waitForPollTick         = 250 * time.Millisecond
)

func waitBudget(deadline time.Time) time.Duration {
	if d := time.Until(deadline); d > waitForPollTick {
		return d
	}
	return waitForPollTick
}

func parseChangesFeed(tableRaw, cursorRaw, limitRaw json.RawMessage) (table, cursor string, limit int, err error) {
	if len(cursorRaw) > 0 {
		var s string
		if e := json.Unmarshal(cursorRaw, &s); e != nil || strings.TrimSpace(s) == "" {
			return "", "", 0, badRequest(`cursor must be a non-empty opaque token, or the literal "begin" — omit the field to start at the current head`)
		}
		cursor = s
	}
	if len(tableRaw) > 0 {
		var s string
		if e := json.Unmarshal(tableRaw, &s); e != nil || normTable(s) == "" {
			return "", "", 0, badRequest("table must be a non-empty table name — omit the field for the namespace-wide feed")
		}
		table = normTable(s)
	}
	limit = store.DefaultChangesPageLimit
	if len(limitRaw) > 0 {
		var n int
		if e := json.Unmarshal(limitRaw, &n); e != nil || n < 1 || n > store.MaxChangesPageLimit {
			return "", "", 0, badRequest("limit must be an integer between 1 and %d (default %d)", store.MaxChangesPageLimit, store.DefaultChangesPageLimit)
		}
		limit = n
	}
	return table, cursor, limit, nil
}

func (s *Server) feedScope(ctx context.Context, ns, table string) (*store.RowScope, store.Incarnation, error) {
	if table == "" {
		return nil, store.Incarnation{}, nil
	}
	return s.resolveScope(ctx, ns, table)
}

func runChangesSince(ctx context.Context, s *Server, op, ns, table, cursor string, limit int, scope *store.RowScope, inc store.Incarnation) ([]store.ChangeRecord, store.Cursor, error) {
	records, next, err := s.eng.ChangesSince(ctx, ns, table, store.Cursor(cursor),
		[16]byte{}, scope, inc, store.Page{Limit: limit})
	if err != nil {

		if errors.Is(err, store.ErrCursorExpired) {
			return nil, "", badRequest("cursor is unknown or past the change-log retention window (-change-retention, default 168h); catch up by calling %s with no cursor to resume from the current head, or with cursor \"begin\" to replay retained history", op)
		}
		if errors.Is(err, store.ErrCursorCrossFeed) {
			return nil, "", badRequest("cursor was minted on a different feed (a specific table's, or the namespace-wide feed); pass it only to the feed you received it from — honoring it elsewhere would silently skip events — or start fresh with no cursor / \"begin\"")
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", wrapStoreErr(err)
	}
	return records, next, nil
}

func renderChanges(records []store.ChangeRecord, next store.Cursor) map[string]any {
	changes := make([]map[string]any, len(records))
	for i, r := range records {
		changes[i] = map[string]any{
			"cursor": string(r.Cursor),
			"table":  r.Table,
			"row_id": r.RowID,
			"kind":   string(r.Kind),
		}
	}
	return map[string]any{"changes": changes, "next_cursor": string(next)}
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
			ns := normNS(req.Namespace)
			if err := s.requireNamespaceVisible(ctx, ns); err != nil {
				return nil, err
			}
			tables, err := s.eng.ListTables(ctx, ns, nil)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			if tables, err = s.visibleTables(ctx, ns, tables); err != nil {
				return nil, err
			}
			if tables == nil {
				tables = []string{}
			}
			return map[string]any{"tables": tables}, nil
		},
	},
	"list_namespaces": {
		Description: "List the namespaces on this server (one isolated SQLite file per namespace). " +
			"Use it to see which namespaces already exist before creating or reusing one. " +
			"An optional prefix (a namespace path) restricts the listing to that path's subtree, " +
			"recursively, the prefix itself included; omit it to list everything.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"prefix": map[string]any{
					"type":        "string",
					"description": "Namespace path whose recursive subtree is listed (the path itself included); omit to list every namespace",
					"pattern":     store.NSPathPattern(),
				},
			},
		},
		OutputSchema: outSchema(map[string]any{
			"namespaces": map[string]any{
				"type":        "array",
				"description": "Namespace names, ordered as their database filenames order them (v0.2.0's order; edge-x before edge)",
				"items":       map[string]any{"type": "string"},
			},
		}, "namespaces"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req listNamespacesReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}

			prefix := ""
			if len(req.Prefix) > 0 {
				var p string
				if err := json.Unmarshal(req.Prefix, &p); err != nil {
					return nil, badRequest("prefix must be a string")
				}
				if p = normNS(p); p == "" {
					return nil, badRequest("prefix must not be empty — omit the field to list every namespace (an empty prefix would silently list everything)")
				}
				prefix = p
			}
			nss, err := s.eng.ListNamespaces(ctx, prefix, nil)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			if nss, err = s.visibleNamespaces(ctx, nss); err != nil {
				return nil, err
			}
			if nss == nil {
				nss = []string{}
			}
			return map[string]any{"namespaces": nss}, nil
		},
	},
	"create_namespace": {
		Description: "Create an empty namespace. A namespace is a path of 1-3 segments (a/b/c), each " +
			"1-64 chars: a leading letter or digit, then [a-z0-9_-]. Parents need not exist as " +
			"namespaces — creating a child makes its parent directories. Namespaces are also created implicitly on first use by the write ops " +
			"(create_table, insert, update, upsert, upsert_by_key, delete, migrate); every read answers not_found for a namespace that does not exist and creates nothing, " +
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
			if err := s.eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"namespace": ns}, nil
		},
	},
	"drop_namespace": {
		Description: "Drop a namespace and every table in it, deleting its SQLite file and WAL sidecars. " +
			"Irreversible. confirm must repeat the namespace name — a guard against dropping the wrong one " +
			"(it normalizes like the namespace itself, so case and surrounding whitespace don't matter). " +
			"In-flight requests on the namespace finish first (or fail); any later write-op use of the same name recreates " +
			"the namespace empty — every read answers not_found until it is recreated. " +
			"The server closes its own connections before deleting, but other processes " +
			"holding the file open (a second dolmen, a backup tool) are not detected — coordinate drops within one server.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace to drop"),
				"confirm": map[string]any{
					"type":        "string",
					"description": "Safety guard: repeat the namespace name here to confirm the irreversible drop (normalized like the namespace itself)",
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
			if err := s.dropNamespaceGrants(ctx, ns); err != nil {
				return nil, err
			}
			if err := s.eng.DropNamespace(ctx, ns, [16]byte{}); err != nil {
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
			ns := normNS(req.Namespace)
			if err := s.dropTableGrants(ctx, ns, table); err != nil {
				return nil, err
			}
			if err := s.eng.DropTable(ctx, ns, table, store.Incarnation{}); err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"dropped": table}, nil
		},
	},
	"describe_server": {
		Description: "Report the server's embedding provider status read-only: provider (none, local, openai), " +
			"model, the identity string that pins vectorized tables to this provider and model, and whether " +
			"server-side embedding is usable (creating vectorize fields, embedding search_vector text queries). " +
			"For the local provider, model_cached reports whether the model weights are complete on the " +
			"server: for a Hugging Face model, false means the first vectorized write or text search downloads " +
			"it from the Hugging Face Hub, which can fail transiently — retry the request, or pre-seed the " +
			"cache; for a configured model directory, false means the directory is incomplete and no download " +
			"repairs it. " +
			"Call it to answer those questions without attempting a write or a text search. Status only — " +
			"no secrets are exposed, no embedding is run, and no network request is made: usable reflects " +
			"configuration, so an endpoint that is down or rejects the request still fails at first use, " +
			"not here.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{},
		},
		OutputSchema: outSchema(map[string]any{
			"embedding": map[string]any{
				"type":        "object",
				"description": "Server-side embedding provider status",
				"properties": map[string]any{
					"provider":     prop("string", "Active embedding provider: none, local (in-process, no external service), or openai (external OpenAI-compatible endpoint)"),
					"model":        prop("string", "Configured model name (present when the provider reports one)"),
					"identity":     prop("string", "Identity string that pins vectorized tables to this provider and model — the value a table's embed_space must match for inserts and text searches (present when the provider reports one; absent means vectorize and text queries are rejected until an operator configures the server)"),
					"usable":       prop("boolean", "Whether server-side embedding is currently usable (creating vectorize fields, embedding text queries): true when the provider is configured and reports its identity; configuration status only — the provider is not called, so a local Hugging Face model that is not yet cached (model_cached false) still downloads on first use rather than failing here"),
					"model_cached": prop("boolean", "Whether the model weights are complete on the server, so the first vectorized write or text search needs no download (local provider only; absent for none and openai): for a Hugging Face model, false means that first use downloads it from the Hugging Face Hub, which can fail transiently — retry the request (a failed write rolls back and consumes no idempotency key) or pre-seed the cache; when DOLMEN_EMBED_MODEL names a model directory, false means the directory is incomplete and no download repairs it — an operator must fix or replace it. An embedder_unavailable error names which case applies"),
				},
				"required":             []string{"provider", "usable"},
				"additionalProperties": false,
			},
		}, "embedding"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req struct{}
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			emb := map[string]any{
				"provider": s.emb.Name(),
				"usable":   s.emb.Identity() != "",
			}
			if m := s.emb.ModelName(); m != "" {
				emb["model"] = m
			}
			if id := s.emb.Identity(); id != "" {
				emb["identity"] = id
			}
			if c, ok := s.emb.(interface{ Cached() bool }); ok {
				emb["model_cached"] = c.Cached()
			}
			return map[string]any{"embedding": emb}, nil
		},
	},
	"capabilities": {
		Description: "Report the storage engine's static capabilities: vector_execution (\"exact\" or \"ann\"), " +
			"ann_recall_bound (explicitly null when execution is exact — never omitted; a number in (0,1] iff ann, " +
			"the guaranteed minimum recall versus the exact path), notifications (whether commit notifications are " +
			"implemented), subscribe (whether live streams are available), query_dialect (the SQL dialect query " +
			"accepts) and filter_dialect (the dialect a filter is read in under auth: off; under auth: on the " +
			"shared allowlist binds instead). Field names and types are pinned, so " +
			"the discovery is portable across conforming engines; unknown future fields are additive. Read-only, " +
			"engine-reported verbatim — the single discovery surface under auth: off, and what describe_server inlines under auth: on.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{},
		},
		OutputSchema: outSchema(map[string]any{
			"vector_execution": map[string]any{
				"type":        "string",
				"description": "How the engine executes vector search: exact (brute-force) or ann (approximate nearest-neighbor within the declared recall bound)",
				"enum":        []string{string(store.VectorExact), string(store.VectorANN)},
			},
			"ann_recall_bound": map[string]any{
				"description": "Guaranteed minimum recall versus the exact path over the conformance corpus: explicitly null when vector_execution is exact (never omitted), a number in (0,1] iff ann",
				"anyOf": []any{
					map[string]any{"type": "number"},
					map[string]any{"type": "null"},
				},
			},
			"notifications":  prop("boolean", "Whether the engine implements commit notifications (wait_for)"),
			"subscribe":      prop("boolean", "Whether the engine serves live change streams"),
			"query_dialect":  prop("string", "The SQL dialect the query operation accepts, named by family (e.g. sqlite, postgresql); an open enum, so branch on it rather than assuming a closed set"),
			"filter_dialect": prop("string", "The SQL dialect a filter expression is read in under auth: off, named the same way; under auth: on every engine reads the shared allowlist instead, and this field is informational"),
		}, "vector_execution", "ann_recall_bound", "notifications", "subscribe", "query_dialect", "filter_dialect"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req struct{}
			if err := decode(body, &req); err != nil {
				return nil, err
			}

			return s.eng.Capabilities(), nil
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
			ns := normNS(req.Namespace)
			scope, inc, err := s.resolveScope(ctx, ns, normTable(req.Table))
			if err != nil {
				return nil, err
			}
			sc, count, err := s.eng.DescribeTable(ctx, ns, normTable(req.Table), scope, inc)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"table": sc, "row_count": count}, nil
		},
	},
	"create_table": {
		Description: "Create a table with typed fields. Types: string, text, number, boolean, timestamp, json, vector. " +
			"Annotations: fulltext=true indexes a string/text field for stemmed full-text search (plural/inflected terms match, e.g. payments <-> payment); type=vector stores caller-provided " +
			"embeddings (dim required); vectorize=true on a string/text field makes the server embed that field automatically " +
			"for vector search; enum=[values] restricts a string field to a closed vocabulary — writes carrying any other value " +
			"are rejected (exact match, no case folding; a declared default must be a member); " +
			"default=<value> is stored by later inserts that omit the field (instead of NULL; must match " +
			"the field's type; not allowed on required or vectorize fields) — a timestamp field may instead declare " +
			"default=now(), stamped with the server's current time on each write that omits the field, so idempotent " +
			"retries can omit it and replay instead of hashing a regenerated client timestamp. Consider infer_schema first when starting from sample records.",
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
					"items": fieldItemSchema("Field definition", true),
				},
			},
			"required": []string{"namespace", "table", "fields"},
		},
		OutputSchema: outSchema(map[string]any{
			"table": tableOutSchema("Schema of the created table"),
		}, "table"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req createTableReq
			var rowAccess string
			if s.authn.On() {
				var ext createTableAuthReq
				if err := decode(body, &ext); err != nil {
					return nil, err
				}
				req = createTableReq{Namespace: ext.Namespace, Table: ext.Table, Fields: ext.Fields}
				rowAccess = ext.RowAccess
			} else if err := decode(body, &req); err != nil {
				return nil, err
			}

			if s.emb.Identity() == "" {
				for _, f := range req.Fields {
					if !f.Vectorize {
						continue
					}
					if err := schema.Validate(schema.Normalize(req.Fields)); err != nil {
						return nil, badRequest("%s", err)
					}
					return nil, badRequest("field %q has vectorize, but this server has no usable embedding provider (none is configured, or the configured one does not report its identity); the table is not created — %s; or create the field without vectorize and enable it via migrate (set_vectorize) once a provider is configured", f.Name, embedProviderHelp)
				}
			}
			ns := normNS(req.Namespace)
			if err := s.ensureNamespace(ctx, ns); err != nil {
				return nil, wrapStoreErr(err)
			}
			sc, err := s.eng.CreateTable(ctx, ns, normTable(req.Table), req.Fields, store.TableOpts{RowAccess: rowAccess}, [16]byte{})
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
			"missing required fields are rejected; fields omitted without a declared default store NULL. " +
			"Full-text and vector indexes update automatically; " +
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

					"pattern": fmt.Sprintf(`^[ -~]{1,%d}$`, store.MaxIdempotencyKeyLen),
				},
			},
			"required": []string{"namespace", "table", "records"},
		},
		OutputSchema: writeOutSchema(false, true),
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

			ns := normNS(req.Namespace)
			if err := s.ensureNamespace(ctx, ns); err != nil {
				return nil, wrapStoreErr(err)
			}
			scope, inc, err := s.resolveScope(ctx, ns, normTable(req.Table))
			if err != nil {
				return nil, err
			}
			wide := false
			if key != "" {
				wide, err = s.holdsTableWideRead(ctx, ns, normTable(req.Table))
				if err != nil {
					return nil, err
				}
			}
			res, err := s.eng.Insert(ctx, ns, normTable(req.Table), req.Records,
				store.WriteOpts{IdempotencyKey: key, Owner: s.writeOwner(ctx), TableWideRead: wide}, s.embedder(), scope, inc)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			if key != "" {
				inserted := len(res.Ids)
				if res.Replayed {
					inserted = 0
				}
				return map[string]any{"ids": res.Ids, "inserted": inserted, "replayed": res.Replayed}, nil
			}
			return map[string]any{"ids": res.Ids, "inserted": len(res.Ids)}, nil
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
		OutputSchema: writeOutSchema(true, false),
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
			ns := normNS(req.Namespace)
			if err := s.ensureNamespace(ctx, ns); err != nil {
				return nil, wrapStoreErr(err)
			}
			scope, inc, err := s.resolveScope(ctx, ns, normTable(req.Table))
			if err != nil {
				return nil, err
			}
			res, err := s.eng.UpsertByKey(ctx, ns, normTable(req.Table), req.On, req.Records,
				store.WriteOpts{Owner: s.writeOwner(ctx)}, s.embedder(), scope, inc)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"ids": res.Ids, "inserted": res.Inserted, "updated": res.Updated}, nil
		},
	},
	"read_rows": {
		Description: "Fetch rows by id: pass the row ids an insert returned, a query projected, or a change feed " +
			"carried, and get the full rows back. The plain by-id read — no SQL to write, no namespace-wide gate to hold. " +
			"ids address a set: each found row appears once, in ascending id order, " +
			"and ids that are missing are simply absent from the response — never an error; row_count reports how many came back. " +
			"truncated is true only when the response budget dropped rows for existing ids (retry with fewer ids) — it never fires for missing ids. " +
			"Results honor declared field types (boolean -> true/false, json -> decoded value, vector -> number array) " +
			"and omit the hidden _embedding column. At most " + strconv.Itoa(store.MaxReadRowsIDs) + " ids per request.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace of the table"),
				"table":     existingTableProp("Table name"),
				"ids": map[string]any{
					"type":        "array",
					"description": "Row ids to fetch; each found row is returned once, in ascending id order — missing ids are simply absent",
					"items":       map[string]any{"type": "integer"},
					"maxItems":    store.MaxReadRowsIDs,
				},
			},
			"required": []string{"namespace", "table", "ids"},
		},
		OutputSchema: outSchema(map[string]any{
			"rows": map[string]any{
				"type":        "array",
				"description": "The found rows in ascending id order, keyed by field name; declared fields honor their types, and the hidden _embedding column is omitted",
				"items":       map[string]any{"type": "object", "description": "Row keyed by field name"},
			},
			"row_count": prop("integer", "Number of rows returned (ids that were missing are absent, never an error)"),
			"truncated": prop("boolean", "True when the response budget dropped rows for existing ids — retry with fewer ids; never true for missing ids"),
		}, "rows", "row_count", "truncated"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req readRowsReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			if req.Ids == nil {
				return nil, badRequest(`ids is required (pass the ids a write returned, a query projected, or a change feed carried; an empty list selects nothing)`)
			}
			ns := normNS(req.Namespace)
			scope, inc, err := s.resolveScope(ctx, ns, normTable(req.Table))
			if err != nil {
				return nil, err
			}
			res, err := s.eng.GetRows(ctx, ns, normTable(req.Table), *req.Ids, scope, inc)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"rows": res.Rows, "row_count": len(res.Rows), "truncated": res.Truncated}, nil
		},
	},
	"query": {
		Description: "Run a read-only SQL statement (SELECT or WITH only) against one namespace. " +
			"Use table and column names from list_tables/describe_table. Bind parameters with ? and pass args. " +
			"Coercion to declared field types is by result-column label, so aliases count as their label: " +
			"a label declared boolean reads true/false, json reads decoded, vector reads a number array, " +
			"number reads integer or float. Labels that match no declared field, or that different tables " +
			"declare with different types, fall back to raw values (blobs as base64). " +
			"id and created_at are included in SELECT *; the hidden _embedding column is omitted and may not be available through caller SQL, depending on the backend. " +
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
			if utf8.RuneCountInString(req.SQL) > store.MaxQueryRunes {
				return nil, badRequest("sql exceeds %d characters", store.MaxQueryRunes)
			}
			ns := normNS(req.Namespace)
			res, err := s.eng.Query(ctx, ns, req.SQL, req.Args, [16]byte{},
				store.Page{Offset: req.Offset, Limit: req.Limit})
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"rows": res.Rows, "row_count": len(res.Rows), "truncated": res.Truncated}, nil
		},
	},
	"search_fulltext": {
		Description: "Full-text search over fields marked fulltext, using SQLite FTS5 MATCH syntax " +
			"(e.g. \"payment\", \"credit refund\", \"status:ok AND retry\"). The index stems English words " +
			"(porter over unicode61), so plural and inflected query terms match (payments <-> payment, refunds <-> refund); " +
			"phrases and prefix terms operate on stems (pay* stems to pai*, matching paid/paying/pays but not payment). " +
			"Returns matching records ordered by relevance (stable rowid tie-breaking). " +
			"Optional filter and args restrict matches to rows satisfying a SQL WHERE expression over the table's columns " +
			"(same semantics as search_vector's filter) before ranking. " +
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
				"filter": map[string]any{
					"type":        "string",
					"description": "Optional SQL WHERE expression over the table's columns, filtering matches before ranking (same semantics as search_vector's filter)",
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
			if err := decodeAllowNullArgs(body, &req); err != nil {
				return nil, err
			}
			if req.Query == "" {
				return nil, badRequest("query must not be empty")
			}
			ns := normNS(req.Namespace)
			scope, inc, tsc, err := s.resolveScopeState(ctx, ns, normTable(req.Table))
			if err != nil {
				return nil, err
			}
			if err := s.checkFilter(tsc, req.Filter, req.Args); err != nil {
				return nil, err
			}
			res, err := s.eng.SearchFulltext(ctx, ns, normTable(req.Table), req.Query, req.Filter, req.Args,
				req.IncludeHidden, scope, inc, store.Page{Offset: req.Offset, Limit: limit(req.Limit)})
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"results": res.Rows, "truncated": res.Truncated}, nil
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
			vq, err := ops.PrepareVectorQuery(ctx, s.eng, normNS(req.Namespace), normTable(req.Table), ops.VectorQuery{
				Column:   req.Column,
				Text:     req.Text,
				Vec:      req.Vector,
				Filter:   req.Filter,
				Args:     req.Args,
				MinScore: req.MinScore,
			}, s.emb, embedProviderHelp)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			scope, inc, tsc, err := s.resolveScopeState(ctx, normNS(req.Namespace), normTable(req.Table))
			if err != nil {
				return nil, err
			}
			if err := s.checkFilter(tsc, req.Filter, req.Args); err != nil {
				return nil, err
			}
			res, err := s.eng.SearchVector(ctx, normNS(req.Namespace), normTable(req.Table), vq,
				req.IncludeHidden, scope, inc, store.Page{Offset: req.Offset, Limit: limit(req.Limit)})
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"results": res.Rows, "truncated": res.Truncated, "skipped_vectors": res.SkippedVectors}, nil
		},
	},

	"changes_since": {
		Description: "Replay the namespace's durable change log: the changes committed after a cursor, " +
			"in commit order, as one bounded page plus the next cursor — the polling-friendly half of dolmen's " +
			"realtime surface (§9). Omit cursor to start at the current head: nothing replays, the response's " +
			"next_cursor is the head cursor, and only subsequent commits are delivered — fresh subscribers miss " +
			"nothing. Pass the literal \"begin\" to replay retained history from the oldest readable boundary, or " +
			"pass the next_cursor (or any change's cursor) from a previous call to resume gap-free. " +
			"An optional table filters the feed to that table's CURRENT lifetime — records from before a " +
			"drop-and-recreate are never replayed; omit it for the namespace-wide feed. Each change carries only " +
			"cursor, table, row_id, and kind (insert/update/delete): re-read current row content by id with query " +
			"(a change is identity, never a row snapshot). A cursor that is unknown or past the change-log retention " +
			"window (default 168h) is rejected with a teaching error: catch up by calling again with no cursor " +
			"(current head) or with \"begin\" (retained history).",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace whose change log to replay"),
				"table": existingTableProp("Optional table filter: replay only this table's current lifetime " +
					"(omitted: every table in the namespace, in one commit-ordered feed)"),
				"cursor": map[string]any{
					"type":        "string",
					"description": "Opaque resume token from a previous response's next_cursor or any change's cursor; the literal \"begin\" replays retained history from the oldest readable boundary; omitted starts at the current head (future commits only)",
					"minLength":   1,
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max changes per page (default 100, max 1000; values outside 1–1000 are invalid_request)",
					"minimum":     1,
					"maximum":     store.MaxChangesPageLimit,
				},
			},
			"required": []string{"namespace"},
		},
		OutputSchema: changesPageOutSchema(),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {

			var req changesSinceReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}

			table, cursor, limit, err := parseChangesFeed(req.Table, req.Cursor, req.Limit)
			if err != nil {
				return nil, err
			}
			ns := normNS(req.Namespace)
			scope, inc, err := s.feedScope(ctx, ns, table)
			if err != nil {
				return nil, err
			}
			records, next, err := runChangesSince(ctx, s, "changes_since", ns, table, cursor, limit, scope, inc)
			if err != nil {
				return nil, err
			}
			return renderChanges(records, next), nil
		},
	},
	"wait_for": {
		Description: "Long-poll the change feed — the wake-up primitive, one plain tool call that blocks server-side: " +
			"wait until a change commits after the cursor, or until timeout_ms elapses, and return exactly a " +
			"changes_since page. Same feed semantics: omit cursor to start at the current head (only subsequent " +
			"commits wake the call), pass the literal \"begin\" to wait atop retained history, or resume from a " +
			"previous next_cursor; an optional table filters the wait to that table's current lifetime. " +
			"timeout_ms bounds the wait (default 30000, max 60000; 0 returns immediately — a cheap conditional " +
			"poll). On timeout the response is an EMPTY page carrying the unchanged cursor — never an error: " +
			"pass next_cursor back in and keep waiting. Prefer this over polling changes_since in a loop — the " +
			"server holds the wait, not your token budget. The teaching errors are changes_since's: a cursor that " +
			"is unknown, past the change-log retention window, or minted on a different feed is rejected naming " +
			"the catch-up path. Like every read, a wait never creates its namespace: a missing one is " +
			"not_found — create it first (create_namespace, or any write op), then wait.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"namespace": nsProp("Namespace whose change feed to wait on"),
				"table": existingTableProp("Optional table filter: wait only on this table's current lifetime " +
					"(omitted: every table in the namespace, one commit-ordered feed)"),
				"cursor": map[string]any{
					"type":        "string",
					"description": "Opaque resume token from a previous response's next_cursor or any change's cursor; the literal \"begin\" waits atop retained history; omitted starts at the current head (future commits only)",
					"minLength":   1,
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max changes per page (default 100, max 1000; values outside 1–1000 are invalid_request)",
					"minimum":     1,
					"maximum":     store.MaxChangesPageLimit,
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("How long to hold the wait, in milliseconds (default %d, max %d; values outside 0–%d are invalid_request). 0 returns immediately — a cheap conditional poll", defaultWaitForTimeoutMS, maxWaitForTimeoutMS, maxWaitForTimeoutMS),
					"minimum":     0,
					"maximum":     maxWaitForTimeoutMS,
				},
			},
			"required": []string{"namespace"},
		},
		OutputSchema: changesPageOutSchema(),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {

			var req waitForReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			table, cursor, limit, err := parseChangesFeed(req.Table, req.Cursor, req.Limit)
			if err != nil {
				return nil, err
			}
			timeoutMS := defaultWaitForTimeoutMS
			if len(req.TimeoutMS) > 0 {
				var n int
				if e := json.Unmarshal(req.TimeoutMS, &n); e != nil || n < 0 || n > maxWaitForTimeoutMS {
					return nil, badRequest("timeout_ms must be an integer between 0 and %d (default %d; 0 returns immediately)", maxWaitForTimeoutMS, defaultWaitForTimeoutMS)
				}
				timeoutMS = n
			}
			ns := normNS(req.Namespace)

			deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)

			scope, inc, err := s.feedScope(ctx, ns, table)
			if err != nil {
				return nil, err
			}
			pinned := scope != nil

			validated := false
			for {
				if !pinned {
					if scope, inc, err = s.feedScope(ctx, ns, table); err != nil {
						return nil, err
					}
				}

				readCtx, cancel := context.WithTimeout(ctx, waitBudget(deadline))
				records, next, err := runChangesSince(readCtx, s, "wait_for", ns, table, cursor, limit, scope, inc)
				cancel()
				if err != nil {

					if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && validated {
						return renderChanges(nil, store.Cursor(cursor)), nil
					}
					return nil, err
				}
				validated = true
				if len(records) > 0 {
					return renderChanges(records, next), nil
				}

				cursor = string(next)
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return renderChanges(records, next), nil
				}

				tick := waitForPollTick
				if remaining < tick {
					tick = remaining
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(tick):
				}
			}
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
			ns := normNS(req.Namespace)
			if err := s.ensureNamespace(ctx, ns); err != nil {
				return nil, wrapStoreErr(err)
			}
			scope, inc, tsc, err := s.resolveScopeState(ctx, ns, normTable(req.Table))
			if err != nil {
				return nil, err
			}
			if err := s.checkFilter(tsc, req.Filter, req.Args); err != nil {
				return nil, err
			}
			res, err := s.eng.Delete(ctx, ns, normTable(req.Table), req.Filter, req.Args, store.DeleteOptions{
				DryRun:  dryRun,
				Limit:   limit,
				Confirm: confirm,
			}, scope, inc)
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
			ns := normNS(req.Namespace)
			if err := s.ensureNamespace(ctx, ns); err != nil {
				return nil, wrapStoreErr(err)
			}
			scope, inc, tsc, err := s.resolveScopeState(ctx, ns, normTable(req.Table))
			if err != nil {
				return nil, err
			}
			if err := s.checkFilter(tsc, req.Filter, req.Args); err != nil {
				return nil, err
			}
			res, err := s.eng.Update(ctx, ns, normTable(req.Table), req.Filter, req.Args, req.Set,
				s.embedder(), scope, inc)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"updated": res.Updated}, nil
		},
	},
	"upsert": {
		Description: "Update rows matching a SQL WHERE expression, or insert one record when no row matches. " +
			"With matches it behaves exactly like update (all matched rows get the set values); with none it " +
			"inserts set as a new record, which must then satisfy required fields. Returns the shared write " +
			"shape: ids of the touched rows (the updated rows in id order, or the new row), inserted (0 or 1), " +
			"and updated.",
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
		OutputSchema: writeOutSchema(true, false),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req updateReq
			if err := decodeData(body, &req); err != nil {
				return nil, err
			}
			ns := normNS(req.Namespace)
			if err := s.ensureNamespace(ctx, ns); err != nil {
				return nil, wrapStoreErr(err)
			}
			scope, inc, tsc, err := s.resolveScopeState(ctx, ns, normTable(req.Table))
			if err != nil {
				return nil, err
			}
			if err := s.checkFilter(tsc, req.Filter, req.Args); err != nil {
				return nil, err
			}
			res, err := s.eng.Upsert(ctx, ns, normTable(req.Table), req.Filter, req.Args, req.Set,
				store.WriteOpts{Owner: s.writeOwner(ctx)}, s.embedder(), scope, inc)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"ids": res.Ids, "inserted": res.Inserted, "updated": res.Updated}, nil
		},
	},
	"migrate": {
		Description: "Evolve a table schema: add_field, rename_field, drop_field, set_fulltext, set_vectorize, set_enum. " +
			"Bumps the schema version and records the change. Adding fulltext rebuilds the search index; " +
			"re-asserting set_fulltext ... = true on an already-indexed field also rebuilds it (the reindex path for " +
			"tables created before stemming became the default); " +
			"enabling vectorize backfills embeddings for existing rows. set_enum replaces a string field's " +
			"vocabulary with the given list (not a delta; an empty list removes the constraint) and rejects " +
			"when a row still stores a dropped value, naming the value and its row count. add_field accepts a default that is " +
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
								"description": "add_field | rename_field | drop_field | set_fulltext | set_vectorize | set_enum",
								"enum":        []string{"add_field", "rename_field", "drop_field", "set_fulltext", "set_vectorize", "set_enum"},
							},
							"field": fieldItemSchema("Field definition for add_field (its backfill default is the change's default, not a field property)", false),
							"from":  existingFieldNameProp("Current name (rename_field)"),
							"to":    fieldNameProp("New name (rename_field)"),
							"name":  existingFieldNameProp("Field name (drop_field, set_fulltext, set_vectorize, set_enum)"),
							"value": prop("boolean", "Flag value (set_fulltext, set_vectorize)"),
							"enum": map[string]any{
								"type":        "array",
								"description": "The field's complete new vocabulary (set_enum only) — not a delta; exact-match string values stored as written. An empty array removes the constraint. Values still stored by rows must all be kept, or the change is rejected naming them and their row counts",
								"items":       map[string]any{"type": "string", "minLength": 1},
								"uniqueItems": true,
							},
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
								"if":   map[string]any{"properties": map[string]any{"op": map[string]any{"const": "set_enum"}}},
								"then": map[string]any{"required": []string{"name", "enum"}},
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
										"op": map[string]any{"not": map[string]any{"enum": []string{"set_fulltext", "set_vectorize"}}},
									},
									"required": []string{"op"},
								},
								"then": map[string]any{"not": map[string]any{"required": []string{"value"}}},
							},
							map[string]any{
								"if": map[string]any{
									"properties": map[string]any{
										"op": map[string]any{"not": map[string]any{"const": "set_enum"}},
									},
									"required": []string{"op"},
								},
								"then": map[string]any{"not": map[string]any{"required": []string{"enum"}}},
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
				"expected_incarnation": prop("string", "Opaque token from a dry run's plan, naming the exact table the plan was made against. Pass it back on apply and the migration is refused if the table was dropped and recreated, or moved on, since the preview. Under authentication a precondition must use this rather than expected_version alone"),
				"dry_run":              prop("boolean", "Validate and preview the migration without applying anything (no writes, no embedding calls)"),
			},
			"required": []string{"namespace", "table", "changes"},
		},
		OutputSchema: migrateOutSchema(
			tableOutSchema("Schema of the migrated table (version bumped); for dry_run, the prospective schema"),
			tableOutSchema("Prospective schema after the changes")),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var shadow struct {
				Namespace           string           `json:"namespace"`
				Table               string           `json:"table"`
				Changes             []map[string]any `json:"changes"`
				ExpectedVersion     *int             `json:"expected_version"`
				ExpectedIncarnation *string          `json:"expected_incarnation"`
				DryRun              bool             `json:"dry_run"`
			}

			if err := decodeData(body, &shadow); err != nil {
				return nil, err
			}
			if err := validateMigrateChanges(shadow.Changes, s.authn.On()); err != nil {
				return nil, err
			}
			var req migrateReq
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			ver := 0
			if req.ExpectedVersion != nil {
				if *req.ExpectedVersion < 1 {
					return nil, badRequest("expected_version must be >= 1")
				}
				ver = *req.ExpectedVersion
			}
			ns := normNS(req.Namespace)
			if err := s.ensureNamespace(ctx, ns); err != nil {
				return nil, wrapStoreErr(err)
			}
			if req.DryRun {
				scope, inc, err := s.resolveScope(ctx, ns, normTable(req.Table))
				if err != nil {
					return nil, err
				}
				plan, err := s.eng.PlanMigration(ctx, ns, normTable(req.Table), req.Changes, s.embedder(),
					store.Incarnation{Version: int64(ver)}, scope, inc)
				if err != nil {
					return nil, wrapStoreErr(err)
				}
				return map[string]any{"table": plan.Table, "dry_run": true, "plan": plan}, nil
			}
			expected := store.Incarnation{Version: int64(ver)}
			if req.ExpectedIncarnation != nil {
				bound, derr := store.DecodeIncarnation(*req.ExpectedIncarnation)
				if derr != nil {
					return nil, wrapStoreErr(derr)
				}
				if ver > 0 && bound.Version != int64(ver) {
					return nil, badRequest("expected_version and expected_incarnation disagree about the schema version; pass the token from the dry run and drop expected_version, or drop the token")
				}
				expected = bound
			} else if s.authn.On() && ver > 0 {
				return nil, badRequest("under authentication, a migration precondition must carry expected_incarnation from a dry run: version 1 cannot tell a table from a same-named predecessor, so expected_version alone would let a plan apply to a table that was dropped and recreated under it")
			}
			sc, err := s.eng.Migrate(ctx, ns, normTable(req.Table), req.Changes, s.embedder(),
				expected)
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
			ns := normNS(req.Namespace)
			ms, err := s.eng.ListMigrations(ctx, ns, normTable(req.Table), store.Incarnation{})
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			return map[string]any{"migrations": ms}, nil
		},
	},
}

const embedProviderHelp = "an operator must set the server-side DOLMEN_EMBED_* environment variables: DOLMEN_EMBED_PROVIDER=local (in-process embeddings, no external service), or DOLMEN_EMBED_PROVIDER=openai plus DOLMEN_EMBED_API_KEY (or OPENAI_API_KEY), optionally DOLMEN_EMBED_BASE_URL and DOLMEN_EMBED_MODEL"

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

type listNamespacesReq struct {
	Prefix json.RawMessage `json:"prefix"`
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

type readRowsReq struct {
	Namespace string   `json:"namespace"`
	Table     string   `json:"table"`
	Ids       *[]int64 `json:"ids"`
}

type queryReq struct {
	Namespace string `json:"namespace"`
	SQL       string `json:"sql"`
	Args      []any  `json:"args"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}

type changesSinceReq struct {
	Namespace string          `json:"namespace"`
	Table     json.RawMessage `json:"table"`
	Cursor    json.RawMessage `json:"cursor"`
	Limit     json.RawMessage `json:"limit"`
}

type waitForReq struct {
	Namespace string          `json:"namespace"`
	Table     json.RawMessage `json:"table"`
	Cursor    json.RawMessage `json:"cursor"`
	Limit     json.RawMessage `json:"limit"`
	TimeoutMS json.RawMessage `json:"timeout_ms"`
}

type ftsReq struct {
	Namespace     string `json:"namespace"`
	Table         string `json:"table"`
	Query         string `json:"query"`
	Offset        int    `json:"offset"`
	Limit         int    `json:"limit"`
	IncludeHidden bool   `json:"include_hidden"`
	Filter        string `json:"filter"`
	Args          []any  `json:"args"`
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
	Namespace           string          `json:"namespace"`
	Table               string          `json:"table"`
	Changes             []schema.Change `json:"changes"`
	ExpectedVersion     *int            `json:"expected_version"`
	ExpectedIncarnation *string         `json:"expected_incarnation"`
	DryRun              bool            `json:"dry_run"`
}
