package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func scopePredicate(scope *store.RowScope, alias string, next int) (string, []any) {
	if scope == nil {
		return "", nil
	}
	if scope.Empty {
		return "false", nil
	}
	col := ident(schema.OwnerColumn)
	if alias != "" {
		col = ident(alias) + "." + col
	}
	return fmt.Sprintf("%s = $%d", col, next), []any{scope.Owner}
}

func scopeUsable(scope *store.RowScope, sc *schema.TableSchema) error {
	if scope == nil || scope.Empty {
		return nil
	}
	if sc != nil && !sc.HasOwner {
		return fmt.Errorf("%w: table %s carries no owner column, so a row scope cannot be applied to it", store.ErrInvalid, sc.Name)
	}
	return nil
}

func (s *Store) guardScope(ctx context.Context, tx pgx.Tx, n namespace, table string, state tableState, want store.Incarnation) error {
	if store.IncarnationIsZero(want) {
		return nil
	}
	if want.Table != table {
		return store.ScopeTableMismatch(want.Table, table)
	}
	if state.incarnation != want {
		return store.ScopeIncarnationChanged(n.name, table)
	}
	return nil
}

const visibleRelation = "_dolmen_visible"

func scopedSource(table string, scope *store.RowScope) (string, string, []any) {
	if scope == nil {
		return "", table, nil
	}
	cond := "false"
	var args []any
	if !scope.Empty {
		cond = ident(schema.OwnerColumn) + " = ?"
		args = []any{scope.Owner}
	}
	return "WITH " + ident(visibleRelation) + " AS MATERIALIZED (SELECT * FROM " + table + " WHERE " + cond + ") ",
		ident(visibleRelation), args
}

func scopedSourceAt(table string, scope *store.RowScope, next int) (string, string, []any) {
	cond, args := scopePredicate(scope, "", next)
	if cond == "" {
		return "", table, nil
	}
	return "WITH " + ident(visibleRelation) + " AS MATERIALIZED (SELECT * FROM " + table + " WHERE " + cond + ") ",
		ident(visibleRelation), args
}
