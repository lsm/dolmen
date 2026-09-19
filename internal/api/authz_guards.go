package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

var errNoGrantRegistry = errors.New("auth is on but no grant registry is configured, so authorization cannot be decided")

func (s *Server) requireGrantableObject(ctx context.Context, obj auth.Object) error {
	if obj.Root() {
		return nil
	}
	if obj.Table == "" {
		if _, err := s.eng.NamespaceState(ctx, obj.Namespace, nil); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return notFound("namespace %s does not exist, so there is nothing to grant on: grants target existing objects, so create the namespace first", obj.Namespace)
			}
			return err
		}
		return nil
	}
	if _, _, err := s.eng.TableState(ctx, obj.Namespace, obj.Table, nil); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFound("table %s does not exist in namespace %s, so there is nothing to grant on: grants target existing objects, so create the table first", obj.Table, obj.Namespace)
		}
		return err
	}
	return nil
}

func (s *Server) guardLastRootAdmin(ctx context.Context, subj auth.Subject, obj auth.Object, verbs auth.VerbSet) error {
	if !obj.Root() || !verbs.Has(auth.VerbAdmin) {
		return nil
	}
	if s.authn.AdminKeyConfigured() {
		return nil
	}
	admins, err := s.grants.RootAdmins(ctx)
	if err != nil {
		return err
	}
	for _, a := range admins {
		if a.Type == auth.SubjectPrincipal && a != subj {
			return nil
		}
	}
	return &Error{
		Status: http.StatusConflict, Code: ErrCodeConflict,
		Message: "revoking this grant would leave the deployment with no usable root administrator, so nobody could grant anything again: grant admin on \"*\" to another principal first, or set DOLMEN_ADMIN_KEY and restart, which restores the bootstrap administrator while the grants persist",
	}
}

func (s *Server) visibleNamespaces(ctx context.Context, nss []string) ([]string, error) {
	if !s.authn.On() {
		return nss, nil
	}
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal {
		return nss, nil
	}
	if s.grants == nil {
		return nil, errNoGrantRegistry
	}
	granted, err := s.grants.NamespacesWithAnyGrant(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, root := granted[auth.RootObject]; root {
		return nss, nil
	}
	out := make([]string, 0, len(nss))
	for _, ns := range nss {
		if namespaceVisible(ns, granted) {
			out = append(out, ns)
		}
	}
	return out, nil
}

func namespaceVisible(ns string, granted map[string]struct{}) bool {
	for g := range granted {
		if g == ns || strings.HasPrefix(ns, g+"/") || strings.HasPrefix(g, ns+"/") {
			return true
		}
	}
	return false
}

func (s *Server) requireNamespaceVisible(ctx context.Context, ns string) error {
	if !s.authn.On() {
		return nil
	}
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal {
		return nil
	}
	if s.grants == nil {
		return errNoGrantRegistry
	}
	granted, err := s.grants.NamespacesWithAnyGrant(ctx, id)
	if err != nil {
		return err
	}
	if _, root := granted[auth.RootObject]; root {
		return nil
	}
	if namespaceVisible(ns, granted) {
		return nil
	}
	return notFound("namespace %s does not exist", ns)
}

func (s *Server) visibleTables(ctx context.Context, ns string, tables []string) ([]string, error) {
	if !s.authn.On() {
		return tables, nil
	}
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal {
		return tables, nil
	}
	if s.grants == nil {
		return nil, errNoGrantRegistry
	}
	nsVerbs, err := s.grants.EffectiveVerbs(ctx, id, auth.Object{Namespace: ns})
	if err != nil {
		return nil, err
	}
	if !nsVerbs.Empty() {
		return tables, nil
	}
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		verbs, err := s.grants.EffectiveVerbs(ctx, id, auth.Object{Namespace: ns, Table: t})
		if err != nil {
			return nil, err
		}
		if !verbs.Empty() {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *Server) ensureNamespace(ctx context.Context, ns string) error {
	if !s.authn.On() {
		return ops.EnsureNamespace(ctx, s.eng, ns)
	}
	if _, err := s.eng.NamespaceState(ctx, ns, nil); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFound("namespace %s does not exist: with auth on, namespaces are never created as a side effect of a write, because that would bypass the admin grant on the parent; ask an administrator to create it with create_namespace", ns)
		}
		return wrapStoreErr(err)
	}
	return nil
}

func (s *Server) authorizeFeed(ctx context.Context, ns, table string) error {
	if !s.authn.On() {
		return nil
	}
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal {
		return nil
	}
	if s.grants == nil {
		return errNoGrantRegistry
	}
	verbs, err := s.grants.EffectiveVerbs(ctx, id, auth.Object{Namespace: normNS(ns), Table: table})
	if err != nil {
		return err
	}
	if verbs.Has(auth.VerbRead) {
		return nil
	}
	return forbidden403()
}

func (s *Server) dropNamespaceGrants(ctx context.Context, ns string) error {
	if s.grants == nil {
		return nil
	}
	return s.grants.DropNamespace(ctx, ns)
}

func (s *Server) dropTableGrants(ctx context.Context, ns, table string) error {
	if s.grants == nil {
		return nil
	}
	return s.grants.DropTable(ctx, ns, table)
}
