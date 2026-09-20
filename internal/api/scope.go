package api

import (
	"context"
	"errors"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Server) resolveScope(ctx context.Context, ns, table string) (*store.RowScope, store.Incarnation, error) {
	if !s.authn.On() {
		return nil, store.Incarnation{}, nil
	}
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal {
		return nil, store.Incarnation{}, nil
	}
	sc, inc, err := s.eng.TableState(ctx, ns, table, nil)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.Incarnation{}, nil
		}
		return nil, store.Incarnation{}, wrapStoreErr(err)
	}
	if s.grants == nil {
		return nil, store.Incarnation{}, errNoGrantRegistry
	}
	verbs, err := s.grants.EffectiveVerbs(ctx, id, auth.Object{Namespace: ns, Table: table})
	if err != nil {
		return nil, store.Incarnation{}, err
	}
	if sc.RowAccess != schema.RowAccessOwn {
		if verbs.HasAny(auth.VerbRead, auth.VerbCreate, auth.VerbUpdate, auth.VerbDelete) {
			return nil, inc, nil
		}
		return &store.RowScope{Empty: true}, inc, nil
	}
	if verbs.Has(auth.VerbRead) {
		return nil, inc, nil
	}
	if verbs.HasAny(auth.VerbCreate, auth.VerbUpdate, auth.VerbDelete) {
		return &store.RowScope{Owner: id.Principal}, inc, nil
	}
	return &store.RowScope{Empty: true}, inc, nil
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
