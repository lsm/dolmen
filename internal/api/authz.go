package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

type authScope int

const (
	scopeNone authScope = iota
	scopeRoot
	scopeParentNamespace
	scopeNamespace
	scopeTable
	scopeTableOrNamespace
	scopeGrantObject
)

type authRule struct {
	Scope   authScope
	Verbs   []auth.Verb
	AnyVerb bool

	OwnRows bool
}

var dataVerbs = []auth.Verb{auth.VerbCreate, auth.VerbUpdate, auth.VerbDelete}

var authRules = map[string]authRule{
	"capabilities":    {Scope: scopeNone},
	"describe_server": {Scope: scopeNone},
	"infer_schema":    {Scope: scopeNone},
	"list_namespaces": {Scope: scopeNone},
	"list_tables":     {Scope: scopeNone},
	"whoami":          {Scope: scopeNone},

	"create_key": {Scope: scopeRoot, Verbs: []auth.Verb{auth.VerbAdmin}},
	"list_keys":  {Scope: scopeRoot, Verbs: []auth.Verb{auth.VerbAdmin}},
	"revoke_key": {Scope: scopeRoot, Verbs: []auth.Verb{auth.VerbAdmin}},

	"rotate_signing_key": {Scope: scopeRoot, Verbs: []auth.Verb{auth.VerbAdmin}},

	"create_namespace": {Scope: scopeParentNamespace, Verbs: []auth.Verb{auth.VerbAdmin}},
	"drop_namespace":   {Scope: scopeNamespace, Verbs: []auth.Verb{auth.VerbAdmin}},
	"vacuum":           {Scope: scopeNamespace, Verbs: []auth.Verb{auth.VerbAdmin}},

	"describe_table": {Scope: scopeTable, AnyVerb: true, Verbs: auth.VerbOrder},
	"tokenize":       {Scope: scopeTable, AnyVerb: true, Verbs: auth.VerbOrder},

	"create_table": {Scope: scopeNamespace, Verbs: []auth.Verb{auth.VerbSchema}},

	"drop_table":      {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbSchema, auth.VerbAdmin}},
	"migrate":         {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbSchema}},
	"list_migrations": {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbSchema}},

	"insert":          {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbCreate}},
	"update":          {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbUpdate}},
	"delete":          {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbDelete}},
	"upsert":          {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbCreate, auth.VerbUpdate}},
	"upsert_by_key":   {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbCreate, auth.VerbUpdate}},
	"read_rows":       {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbRead}, OwnRows: true},
	"search_fulltext": {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbRead}, OwnRows: true},
	"search_vector":   {Scope: scopeTable, Verbs: []auth.Verb{auth.VerbRead}, OwnRows: true},

	"query": {Scope: scopeNamespace, Verbs: []auth.Verb{auth.VerbRead}},

	"changes_since": {Scope: scopeTableOrNamespace, Verbs: []auth.Verb{auth.VerbRead}, OwnRows: true},
	"wait_for":      {Scope: scopeTableOrNamespace, Verbs: []auth.Verb{auth.VerbRead}, OwnRows: true},

	"grant":       {Scope: scopeGrantObject, Verbs: []auth.Verb{auth.VerbAdmin}},
	"revoke":      {Scope: scopeGrantObject, Verbs: []auth.Verb{auth.VerbAdmin}},
	"list_grants": {Scope: scopeGrantObject, Verbs: []auth.Verb{auth.VerbAdmin}},
}

type authTarget struct {
	Namespace string          `json:"namespace"`
	Table     string          `json:"table"`
	Object    *grantObjectRaw `json:"object"`
	Changes   []struct {
		Op    string `json:"op"`
		Value *bool  `json:"value"`
	} `json:"changes"`
}

func disablesRowAccess(t authTarget) bool {
	for _, c := range t.Changes {
		if c.Op == schema.OpSetRowAccess && c.Value != nil && !*c.Value {
			return true
		}
	}
	return false
}

type grantObjectRaw struct {
	Namespace string `json:"namespace"`
	Table     string `json:"table"`
}

func forbidden403() error {
	return derr.New(derr.Forbidden, "%s", forbiddenMessage)
}

const forbiddenMessage = "the caller holds no grant permitting this operation on this object; an administrator grants access with the grant op, and whoami reports the principal and groups this request authenticated as"

func parseAuthTarget(body []byte) authTarget {
	var t authTarget
	if len(body) == 0 {
		return t
	}
	_ = json.Unmarshal(body, &t)
	return t
}

func parentNamespace(ns string) auth.Object {
	idx := strings.LastIndex(ns, "/")
	if idx < 0 {
		return auth.Object{Namespace: auth.RootObject}
	}
	return auth.Object{Namespace: ns[:idx]}
}

func (s *Server) authorizeOp(ctx context.Context, op string, body []byte) error {
	if !s.authn.On() {
		return nil
	}
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal {
		return nil
	}
	rule, ok := authRules[op]
	if !ok {
		return fmt.Errorf("operation %s has no authorization rule", op)
	}
	if rule.Scope == scopeNone {
		return nil
	}
	if s.grants == nil {
		return fmt.Errorf("auth is on but no grant registry is configured, so authorization cannot be decided")
	}

	target := parseAuthTarget(body)
	obj, err := objectFor(rule.Scope, target)
	if err != nil {
		return err
	}

	held, err := s.grants.EffectiveVerbs(ctx, id, obj)
	if err != nil {
		return err
	}

	required := rule.Verbs
	if rule.AnyVerb {
		if held.HasAny(required...) {
			return nil
		}
		return forbidden403()
	}
	if !held.HasAll(required...) {
		if rule.OwnRows && held.HasAny(dataVerbs...) && s.tableHasRowAccess(ctx, obj) {
			return nil
		}
		if op == "query" {
			return derr.New(derr.Forbidden, "%s; query needs read on the whole namespace, not on one table, because its SQL can reach every table in it, so read the table with read_rows or a search instead, or ask for read on the namespace", forbiddenMessage)
		}
		return forbidden403()
	}
	if op == "migrate" {
		if migrationReadsRows(body) && !held.Has(auth.VerbRead) {
			return derr.New(derr.Forbidden, "this migration's outcome depends on the table's existing rows, so it requires the read verb in addition to schema; without it a schema-only caller could learn about rows they cannot see")
		}
		if disablesRowAccess(target) && !held.Has(auth.VerbAdmin) {
			return derr.New(derr.Forbidden, "turning row_access off widens every data-verb holder's reach from their own rows to all owners' rows, which changes what other callers may do, so it requires the admin verb in addition to schema and read")
		}
	}
	return nil
}

func migrationReadsRows(body []byte) bool {
	var req migrateReq
	if err := decode(body, &req); err != nil {
		return true
	}
	for _, c := range req.Changes {
		if c.ReadsRows() {
			return true
		}
	}
	return false
}

func objectFor(scope authScope, t authTarget) (auth.Object, error) {
	switch scope {
	case scopeRoot:
		return auth.Object{Namespace: auth.RootObject}, nil
	case scopeParentNamespace:
		if t.Namespace == "" {
			return auth.Object{}, badRequest("namespace is required")
		}
		return parentNamespace(t.Namespace), nil
	case scopeNamespace:
		if t.Namespace == "" {
			return auth.Object{}, badRequest("namespace is required")
		}
		return auth.Object{Namespace: t.Namespace}, nil
	case scopeTable:
		if t.Namespace == "" {
			return auth.Object{}, badRequest("namespace is required")
		}
		return auth.Object{Namespace: t.Namespace, Table: t.Table}, nil
	case scopeTableOrNamespace:
		if t.Namespace == "" {
			return auth.Object{}, badRequest("namespace is required")
		}
		return auth.Object{Namespace: t.Namespace, Table: t.Table}, nil
	case scopeGrantObject:
		if t.Object == nil {
			return auth.Object{Namespace: auth.RootObject}, nil
		}
		return auth.Object{Namespace: t.Object.Namespace, Table: t.Object.Table}, nil
	}
	return auth.Object{Namespace: auth.RootObject}, nil
}
