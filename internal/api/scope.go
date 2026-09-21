package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Server) resolveScope(ctx context.Context, ns, table string) (*store.RowScope, store.Incarnation, error) {
	scope, inc, _, err := s.resolveScopeState(ctx, ns, table)
	return scope, inc, err
}

func (s *Server) resolveScopeState(ctx context.Context, ns, table string) (*store.RowScope, store.Incarnation, *schema.TableSchema, error) {
	if !s.authn.On() {
		return nil, store.Incarnation{}, nil, nil
	}
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal {
		return nil, store.Incarnation{}, nil, nil
	}
	sc, inc, err := s.eng.TableState(ctx, ns, table, nil)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.Incarnation{}, nil, nil
		}
		return nil, store.Incarnation{}, nil, wrapStoreErr(err)
	}
	if s.grants == nil {
		return nil, store.Incarnation{}, nil, errNoGrantRegistry
	}
	verbs, err := s.grants.EffectiveVerbs(ctx, id, auth.Object{Namespace: ns, Table: table})
	if err != nil {
		return nil, store.Incarnation{}, nil, err
	}
	if sc.RowAccess != schema.RowAccessOwn {
		if verbs.Has(auth.VerbRead) {
			return nil, inc, sc, nil
		}
		if verbs.HasAny(auth.VerbCreate, auth.VerbUpdate, auth.VerbDelete) {
			if sc.HasOwner {
				return &store.RowScope{Empty: true}, inc, sc, nil
			}
			return nil, inc, sc, nil
		}
		return &store.RowScope{Empty: true}, inc, sc, nil
	}
	if verbs.Has(auth.VerbRead) {
		return nil, inc, sc, nil
	}
	if verbs.HasAny(auth.VerbCreate, auth.VerbUpdate, auth.VerbDelete) {
		return &store.RowScope{Owner: id.Principal}, inc, sc, nil
	}
	return &store.RowScope{Empty: true}, inc, sc, nil
}

func (s *Server) writeOwner(ctx context.Context) string {
	if !s.authn.On() {
		return ""
	}
	return auth.IdentityFrom(ctx).Principal
}

func (s *Server) tableHasRowAccess(ctx context.Context, obj auth.Object) bool {
	if obj.Table == "" {
		return false
	}
	sc, _, err := s.eng.TableState(ctx, obj.Namespace, obj.Table, nil)
	if err != nil {
		return false
	}
	return sc.RowAccess == schema.RowAccessOwn
}

func (s *Server) liveAuthz(r *http.Request, ns string) func(table string) (*store.RowScope, store.Incarnation, bool) {
	if !s.authn.On() {
		return nil
	}
	return func(table string) (*store.RowScope, store.Incarnation, bool) {
		id, err := s.authn.Authenticate(r)
		if err != nil {
			return nil, store.Incarnation{}, false
		}
		ctx := auth.WithIdentity(r.Context(), id)
		if err := s.authorizeFeed(ctx, ns, table); err != nil {
			return nil, store.Incarnation{}, false
		}
		if table == "" {
			return nil, store.Incarnation{}, true
		}
		scope, inc, err := s.resolveScope(ctx, ns, table)
		if err != nil {
			return nil, store.Incarnation{}, false
		}
		return scope, inc, true
	}
}
