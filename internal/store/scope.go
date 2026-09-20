package store

import (
	"fmt"

	"github.com/lsm/dolmen/internal/schema"
)

func (sc *RowScope) sql(alias string) (string, []any) {
	if sc == nil {
		return "", nil
	}
	if sc.Empty {
		return "0", nil
	}
	col := q(schema.OwnerColumn)
	if alias != "" {
		col = alias + "." + col
	}
	return fmt.Sprintf("%s = ?", col), []any{sc.Owner}
}

func scopeClause(sc *RowScope, alias string) (string, []any) {
	return sc.sql(alias)
}

func andScope(where string, args []any, sc *RowScope, alias string) (string, []any) {
	clause, sargs := scopeClause(sc, alias)
	if clause == "" {
		return where, args
	}
	combined := append(append([]any(nil), sargs...), args...)
	if where == "" {
		return clause, combined
	}
	return "(" + clause + ") AND (" + where + ")", combined
}

func (s *Store) scopedTable(sc *RowScope, table string) (string, []any) {
	clause, args := scopeClause(sc, "")
	if clause == "" {
		return q(table), nil
	}
	return fmt.Sprintf("(SELECT * FROM %s WHERE %s)", q(table), clause), args
}

func scopeUsable(sc *RowScope, tsc *schema.TableSchema) error {
	if sc == nil || sc.Empty {
		return nil
	}
	if tsc != nil && !tsc.HasOwner {
		return fmt.Errorf("%w: table %s carries no owner column, so a row scope cannot be applied to it", ErrInvalid, tsc.Name)
	}
	return nil
}

var errScopedFilterUnsupported = fmt.Errorf("%w: this request is scoped to your own rows, and a filter expression is not yet accepted on a scoped call; select rows with the operation's structured arguments instead, or ask for the read verb on the table, which lifts the scope", ErrInvalid)
