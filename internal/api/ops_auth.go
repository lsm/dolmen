package api

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/schema"
)

var authOps = map[string]OpDef{}

func (s *Server) authOpsEnabled() bool { return s.authn.On() }

func (s *Server) OpNames() []string {
	names := make([]string, 0, len(Ops)+len(authOps))
	for name := range Ops {
		names = append(names, name)
	}
	if s.authOpsEnabled() {
		for name := range authOps {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *Server) Op(name string) (OpDef, bool) {
	if def, ok := Ops[name]; ok {
		if s.authOpsEnabled() {
			switch name {
			case "create_table":
				return withRowAccessInput(def), true
			case "migrate":
				return withRowAccessChange(def), true
			}
		}
		return def, true
	}
	if s.authOpsEnabled() {
		if def, ok := authOps[name]; ok {
			return def, true
		}
	}
	return OpDef{}, false
}

func withRowAccessInput(def OpDef) OpDef {
	props, ok := def.InputSchema["properties"].(map[string]any)
	if !ok {
		return def
	}
	nextProps := make(map[string]any, len(props)+1)
	for k, v := range props {
		nextProps[k] = v
	}
	nextProps["row_access"] = map[string]any{
		"type":        "string",
		"enum":        []string{schema.RowAccessOwn},
		"description": "Restrict row visibility to the principal who wrote each row. Omit for a table every grant holder sees in full. The server stamps an implicit owner column; callers never supply it, and it cannot be enabled later on a table that already has rows",
	}
	next := make(map[string]any, len(def.InputSchema))
	for k, v := range def.InputSchema {
		next[k] = v
	}
	next["properties"] = nextProps
	def.InputSchema = next
	return def
}

func withRowAccessChange(def OpDef) OpDef {
	props, ok := def.InputSchema["properties"].(map[string]any)
	if !ok {
		return def
	}
	changes, ok := props["changes"].(map[string]any)
	if !ok {
		return def
	}
	items, ok := changes["items"].(map[string]any)
	if !ok {
		return def
	}
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		return def
	}
	opProp, ok := itemProps["op"].(map[string]any)
	if !ok {
		return def
	}
	names, ok := opProp["enum"].([]string)
	if !ok {
		return def
	}

	nextOp := make(map[string]any, len(opProp))
	for k, v := range opProp {
		nextOp[k] = v
	}
	nextOp["enum"] = append(append([]string(nil), names...), schema.OpSetRowAccess)
	nextOp["description"] = "add_field | rename_field | drop_field | set_fulltext | set_vectorize | set_enum | set_row_access"

	nextItemProps := make(map[string]any, len(itemProps))
	for k, v := range itemProps {
		nextItemProps[k] = v
	}
	nextItemProps["op"] = nextOp
	nextItemProps["value"] = prop("boolean", "Flag value (set_fulltext, set_vectorize, set_row_access)")

	nextItems := make(map[string]any, len(items))
	for k, v := range items {
		nextItems[k] = v
	}
	nextItems["properties"] = nextItemProps
	if clauses, ok := items["allOf"].([]any); ok {
		nextItems["allOf"] = withRowAccessConditionals(clauses)
	}

	nextChanges := make(map[string]any, len(changes))
	for k, v := range changes {
		nextChanges[k] = v
	}
	nextChanges["items"] = nextItems

	nextProps := make(map[string]any, len(props))
	for k, v := range props {
		nextProps[k] = v
	}
	nextProps["changes"] = nextChanges

	next := make(map[string]any, len(def.InputSchema))
	for k, v := range def.InputSchema {
		next[k] = v
	}
	next["properties"] = nextProps
	def.InputSchema = next
	return def
}

func withRowAccessConditionals(clauses []any) []any {
	out := make([]any, 0, len(clauses)+1)
	for _, raw := range clauses {
		clause, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		out = append(out, widenValueClause(clause))
	}
	return append(out, map[string]any{
		"if": map[string]any{
			"properties": map[string]any{
				"op": map[string]any{"const": schema.OpSetRowAccess},
			},
			"required": []string{"op"},
		},
		"then": map[string]any{"required": []string{"value"}},
	})
}

func widenValueClause(clause map[string]any) map[string]any {
	cond, ok := clause["if"].(map[string]any)
	if !ok {
		return clause
	}
	props, ok := cond["properties"].(map[string]any)
	if !ok {
		return clause
	}
	opCond, ok := props["op"].(map[string]any)
	if !ok {
		return clause
	}
	not, ok := opCond["not"].(map[string]any)
	if !ok {
		return clause
	}
	names, ok := not["enum"].([]string)
	if !ok || len(names) != 2 || names[0] != schema.OpSetFulltext || names[1] != schema.OpSetVectorize {
		return clause
	}

	nextNot := map[string]any{"enum": []string{schema.OpSetFulltext, schema.OpSetVectorize, schema.OpSetRowAccess}}
	nextOp := make(map[string]any, len(opCond))
	for k, v := range opCond {
		nextOp[k] = v
	}
	nextOp["not"] = nextNot

	nextProps := make(map[string]any, len(props))
	for k, v := range props {
		nextProps[k] = v
	}
	nextProps["op"] = nextOp

	nextCond := make(map[string]any, len(cond))
	for k, v := range cond {
		nextCond[k] = v
	}
	nextCond["properties"] = nextProps

	next := make(map[string]any, len(clause))
	for k, v := range clause {
		next[k] = v
	}
	next["if"] = nextCond
	return next
}

func AuthOpNames() []string {
	names := make([]string, 0, len(authOps))
	for name := range authOps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func subjectSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"type", "id"},
		"description":          "Who the grant is for: a principal, or a group that any identity source may assert",
		"properties": map[string]any{
			"type": map[string]any{"type": "string", "enum": []string{auth.SubjectPrincipal, auth.SubjectGroup}, "description": "principal or group; the two never collide"},
			"id":   prop("string", "The principal string or group name, matched exactly"),
		},
	}
}

func grantObjectSchema(desc string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"namespace"},
		"description":          desc,
		"properties": map[string]any{
			"namespace": prop("string", "Namespace path, or \"*\" for the whole server (the only wildcard, whole-object only)"),
			"table":     prop("string", "Optional table inside the namespace; omit to target the namespace and everything under it"),
		},
	}
}

func verbsSchema() map[string]any {
	return map[string]any{
		"type":        "array",
		"minItems":    1,
		"description": "Verbs to grant or revoke. Responses always serialize them in the order create, read, update, delete, schema, admin",
		"items":       map[string]any{"type": "string", "enum": auth.NewVerbSet(auth.VerbOrder...).Strings()},
	}
}

func grantSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subject":    subjectSchema(),
			"object":     grantObjectSchema("What the grant covers"),
			"verbs":      verbsSchema(),
			"created_at": prop("string", "When the grant was first created, RFC 3339; preserved when verbs are merged into it"),
		},
	}
}

func grantPayload(g auth.Grant) map[string]any {
	return map[string]any{
		"subject":    map[string]any{"type": g.Subject.Type, "id": g.Subject.ID},
		"object":     objectPayload(g.Object),
		"verbs":      g.Verbs.Strings(),
		"created_at": g.CreatedAt.Format(time.RFC3339Nano),
	}
}

func objectPayload(o auth.Object) map[string]any {
	out := map[string]any{"namespace": o.Namespace}
	if o.Table != "" {
		out["table"] = o.Table
	}
	return out
}

type grantRequest struct {
	Subject struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"subject"`
	Object struct {
		Namespace string `json:"namespace"`
		Table     string `json:"table"`
	} `json:"object"`
	Verbs []string `json:"verbs"`
}

func (r grantRequest) parse() (auth.Subject, auth.Object, auth.VerbSet, error) {
	subj := auth.Subject{Type: r.Subject.Type, ID: r.Subject.ID}
	if err := subj.Valid(); err != nil {
		return auth.Subject{}, auth.Object{}, 0, badRequest("%s", err.Error())
	}
	obj := auth.Object{Namespace: r.Object.Namespace, Table: r.Object.Table}
	if obj.Namespace == "" {
		return auth.Subject{}, auth.Object{}, 0, badRequest("object.namespace is required: name a namespace path, or \"*\" for the whole server")
	}
	if obj.Root() && obj.Table != "" {
		return auth.Subject{}, auth.Object{}, 0, badRequest("object {\"namespace\": \"*\", \"table\": %q} is not a grantable object: a table grant needs a concrete namespace, and \"*\" already covers every table", obj.Table)
	}
	verbs, err := auth.ParseVerbs(r.Verbs)
	if err != nil {
		return auth.Subject{}, auth.Object{}, 0, badRequest("%s", err.Error())
	}
	return subj, obj, verbs, nil
}

func init() {
	authOps["whoami"] = OpDef{
		Description: "Report the principal and groups this request authenticated as, and which identity source produced them. " +
			"Self-description only: it grants nothing and reveals nothing about other identities. After a 403, this is how an agent " +
			"finds out who it is before asking an administrator for a grant.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{},
		},
		OutputSchema: outSchema(map[string]any{
			"principal": prop("string", "The caller's principal, exactly as the source produced it"),
			"groups": map[string]any{
				"type":        "array",
				"description": "The caller's groups, normalized: trimmed, empties dropped, repeats removed, order preserved",
				"items":       map[string]any{"type": "string"},
			},
			"source": prop("string", "Which identity source authenticated this request"),
		}, "principal", "groups", "source"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req struct{}
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			id := auth.IdentityFrom(ctx)
			groups := id.Groups
			if groups == nil {
				groups = []string{}
			}
			return map[string]any{"principal": id.Principal, "groups": groups, "source": id.Source}, nil
		},
	}

	authOps["grant"] = OpDef{
		Description: "Give a principal or group verbs on an object. Grants inherit downward: a namespace grant covers every table and " +
			"sub-namespace under it, and \"*\" covers the whole server. Idempotent by (subject, object) — re-granting merges new verbs " +
			"into the existing grant and keeps its created_at. Requires admin on the target object or an ancestor.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"subject", "object", "verbs"},
			"properties": map[string]any{
				"subject": subjectSchema(),
				"object":  grantObjectSchema("What to grant access to: a namespace (and everything under it), one table, or \"*\" for the whole server"),
				"verbs":   verbsSchema(),
			},
		},
		OutputSchema: outSchema(map[string]any{"grant": grantSchema()}, "grant"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req grantRequest
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			subj, obj, verbs, err := req.parse()
			if err != nil {
				return nil, err
			}
			if err := s.requireGrantableObject(ctx, obj); err != nil {
				return nil, err
			}
			g, err := s.grants.Grant(ctx, subj, obj, verbs)
			if err != nil {
				return nil, err
			}
			return map[string]any{"grant": grantPayload(g)}, nil
		},
	}

	authOps["revoke"] = OpDef{
		Description: "Take verbs away from a principal or group on an object. Verbs are explicit — there is no implicit \"all\" — and " +
			"revoking verbs the grant does not hold succeeds unchanged. When the last verb goes the grant ceases to exist and the " +
			"response carries a null grant. Refused when it would leave the deployment with no usable root administrator.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"subject", "object", "verbs"},
			"properties": map[string]any{
				"subject": subjectSchema(),
				"object":  grantObjectSchema("The object the grant covers"),
				"verbs":   verbsSchema(),
			},
		},
		OutputSchema: outSchema(map[string]any{"grant": map[string]any{
			"description": "The grant as it now stands, or null when its last verb was revoked",
			"anyOf":       []any{grantSchema(), map[string]any{"type": "null"}},
		}}, "grant"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req grantRequest
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			subj, obj, verbs, err := req.parse()
			if err != nil {
				return nil, err
			}
			g, err := s.grants.Revoke(ctx, subj, obj, verbs, !s.authn.AdminKeyConfigured())
			if err != nil {
				if errors.Is(err, auth.ErrLastRootAdmin) {
					return nil, lastRootAdminError()
				}
				return nil, err
			}
			if g == nil {
				return map[string]any{"grant": nil}, nil
			}
			return map[string]any{"grant": grantPayload(*g)}, nil
		},
	}

	authOps["list_grants"] = OpDef{
		Description: "List grants, optionally filtered by subject (exact) or object (the named object and everything under it, following " +
			"inheritance). Sorted by subject type, subject id, namespace path, then table. Requires admin on the queried subtree, or on \"*\" when unfiltered.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"subject": subjectSchema(),
				"object":  grantObjectSchema("Limit the listing to this object and everything under it"),
			},
		},
		OutputSchema: outSchema(map[string]any{"grants": map[string]any{
			"type":  "array",
			"items": grantSchema(),
		}}, "grants"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req struct {
				Subject *struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				} `json:"subject"`
				Object *struct {
					Namespace string `json:"namespace"`
					Table     string `json:"table"`
				} `json:"object"`
			}
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			var subj *auth.Subject
			if req.Subject != nil {
				s := auth.Subject{Type: req.Subject.Type, ID: req.Subject.ID}
				if err := s.Valid(); err != nil {
					return nil, badRequest("%s", err.Error())
				}
				subj = &s
			}
			var obj *auth.Object
			if req.Object != nil {
				o := auth.Object{Namespace: req.Object.Namespace, Table: req.Object.Table}
				if o.Namespace == "" {
					return nil, badRequest("object.namespace is required when object is given: name a namespace path, or \"*\" for the whole server")
				}
				obj = &o
			}
			grants, err := s.grants.List(ctx, subj, obj)
			if err != nil {
				return nil, err
			}
			out := make([]any, 0, len(grants))
			for _, g := range grants {
				out = append(out, grantPayload(g))
			}
			return map[string]any{"grants": out}, nil
		},
	}
}
