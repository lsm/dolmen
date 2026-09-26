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

func (s *Server) opAvailable(name string) bool {
	if name != "rotate_signing_key" {
		return true
	}
	_, ok := s.oidc()
	return ok
}

func (s *Server) OpNames() []string {
	names := make([]string, 0, len(Ops)+len(authOps))
	for name := range Ops {
		names = append(names, name)
	}
	if s.authOpsEnabled() {
		for name := range authOps {
			if s.opAvailable(name) {
				names = append(names, name)
			}
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
	if s.authOpsEnabled() && s.opAvailable(name) {
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
	nextOp["description"] = "add_field | rename_field | drop_field | set_fulltext | set_vectorize | set_enum | set_shape | set_row_access"

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
		"description": "Verbs to grant or revoke. Responses always serialize them in the order create, read, update, delete, schema, admin, reveal. admin does not imply reveal, which alone returns secret fields in plaintext",
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
			g, err := s.grants.Revoke(ctx, subj, obj, verbs, !s.authn.AdminKeyConfigured(), s.authn.Reach())
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

func keySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":        prop("string", "Server-generated key id, unique and never reused; revoke_key selects by this"),
			"name":      prop("string", "The name the administrator gave the key; names need not be unique"),
			"principal": prop("string", "The identity this key authenticates as"),
			"groups": map[string]any{
				"type":        "array",
				"description": "The groups this key carries, matched by group grants",
				"items":       map[string]any{"type": "string"},
			},
			"revoked":    prop("boolean", "Whether the key has been revoked"),
			"created_at": prop("string", "When the key was minted, RFC 3339"),
		},
	}
}

func keyPayload(k auth.Key) map[string]any {
	groups := k.Groups
	if groups == nil {
		groups = []string{}
	}
	return map[string]any{
		"id":         k.ID,
		"name":       k.Name,
		"principal":  k.Principal,
		"groups":     groups,
		"revoked":    k.Revoked,
		"created_at": k.CreatedAt.Format(time.RFC3339Nano),
	}
}

func init() {
	authOps["create_key"] = OpDef{
		Description: "Mint an API key that authenticates as a principal, for a machine that cannot do an interactive sign-in. " +
			"The key is returned in full exactly once — it is stored hashed and can never be shown again. It grants nothing by " +
			"itself: grant verbs to its principal or groups separately. Requires admin on \"*\".",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"name", "principal"},
			"properties": map[string]any{
				"name":      prop("string", "A name to tell this key apart in list_keys; need not be unique"),
				"principal": prop("string", "The identity the key authenticates as; grants are made to it separately"),
				"groups": map[string]any{
					"type":        "array",
					"description": "Optional groups the key carries, so group grants apply to it",
					"items":       map[string]any{"type": "string"},
				},
			},
		},
		OutputSchema: outSchema(map[string]any{
			"key":    keySchema(),
			"secret": prop("string", "The credential, shown this once only: present it as Authorization: Bearer <secret>"),
		}, "key", "secret"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req struct {
				Name      string   `json:"name"`
				Principal string   `json:"principal"`
				Groups    []string `json:"groups"`
			}
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			if err := auth.ValidateKeyName(req.Name); err != nil {
				return nil, badRequest("%s", err.Error())
			}
			if err := auth.ValidateKeyIdentity(req.Principal, req.Groups, s.authn.MaxGroups()); err != nil {
				return nil, badRequest("%s", err.Error())
			}
			if s.grants == nil {
				return nil, errNoGrantRegistry
			}
			k, secret, err := s.grants.CreateKey(ctx, req.Name, req.Principal, req.Groups)
			if err != nil {
				return nil, err
			}
			return map[string]any{"key": keyPayload(k), "secret": secret}, nil
		},
	}

	authOps["list_keys"] = OpDef{
		Description: "List the API keys this deployment holds: ids, names, principals, groups and whether each is revoked. " +
			"Never returns credentials — a key's secret is shown once, at creation. Requires admin on \"*\".",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{},
		},
		OutputSchema: outSchema(map[string]any{
			"keys": map[string]any{"type": "array", "items": keySchema()},
		}, "keys"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req struct{}
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			if s.grants == nil {
				return nil, errNoGrantRegistry
			}
			keys, err := s.grants.ListKeys(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]any, 0, len(keys))
			for _, k := range keys {
				out = append(out, keyPayload(k))
			}
			return map[string]any{"keys": out}, nil
		},
	}

	authOps["revoke_key"] = OpDef{
		Description: "Revoke one API key by its id, so it stops authenticating. Selecting by id rather than name means two keys " +
			"sharing a name and principal stay individually revocable. Revoking an already-revoked or unknown key succeeds " +
			"unchanged. Refused when it would leave the deployment with no usable root administrator. Requires admin on \"*\".",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"id"},
			"properties": map[string]any{
				"id": prop("string", "The key id from create_key or list_keys"),
			},
		},
		OutputSchema: outSchema(map[string]any{
			"key": map[string]any{
				"description": "The revoked key, or null when no key has that id",
				"anyOf":       []any{keySchema(), map[string]any{"type": "null"}},
			},
		}, "key"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req struct {
				ID string `json:"id"`
			}
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			if req.ID == "" {
				return nil, badRequest("id is required: name the key to revoke, as create_key and list_keys report it")
			}
			if s.grants == nil {
				return nil, errNoGrantRegistry
			}
			k, err := s.grants.RevokeKey(ctx, req.ID, !s.authn.AdminKeyConfigured(), s.authn.Reach())
			if err != nil {
				if errors.Is(err, auth.ErrLastRootKey) {
					return nil, lastRootKeyError()
				}
				return nil, err
			}
			if k.ID == "" {
				return map[string]any{"key": nil}, nil
			}
			return map[string]any{"key": keyPayload(k)}, nil
		},
	}
}

func init() {
	authOps["rotate_signing_key"] = OpDef{
		Description: "Mint a new token signing key. Tokens issued from now on carry it. By default the predecessor keeps " +
			"verifying, so tokens already in people's hands stay valid until they expire; pass retire_previous to revoke " +
			"them immediately, which is how this deployment signs every human out at once. Requires admin on \"*\", and " +
			"only exists when native sign-in is configured.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"retire_previous": prop("boolean", "Stop honouring tokens signed by earlier keys, signing everyone out at once (default false)"),
			},
		},
		OutputSchema: outSchema(map[string]any{
			"key_id":           prop("string", "The new signing key's id, which appears in the kid header of tokens minted from now on"),
			"retired_previous": prop("boolean", "Whether earlier keys stopped verifying"),
		}, "key_id", "retired_previous"),
		Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
			var req struct {
				RetirePrevious bool `json:"retire_previous"`
			}
			if err := decode(body, &req); err != nil {
				return nil, err
			}
			src, ok := s.oidc()
			if !ok {
				return nil, notFound("unknown operation %q", "rotate_signing_key")
			}
			ring, err := src.Rotate(ctx, req.RetirePrevious)
			if err != nil {
				return nil, err
			}
			s.authn.UseTokens(ring)
			return map[string]any{"key_id": ring.Active.ID, "retired_previous": req.RetirePrevious}, nil
		},
	}
}
