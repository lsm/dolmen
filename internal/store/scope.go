package store

import (
	"context"
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

func scopeUsable(sc *RowScope, tsc *schema.TableSchema) error {
	if sc == nil || sc.Empty {
		return nil
	}
	if tsc != nil && !tsc.HasOwner {
		return fmt.Errorf("%w: table %s carries no owner column, so a row scope cannot be applied to it", ErrInvalid, tsc.Name)
	}
	return nil
}

const visibleRelation = "_dolmen_visible"

func scopedSource(table string, scope *RowScope) (string, string, []any) {
	clause, args := scopeClause(scope, "")
	if clause == "" {
		return "", q(table), nil
	}
	return fmt.Sprintf("WITH %s AS MATERIALIZED (SELECT * FROM %s WHERE %s) ", visibleRelation, q(table), clause),
		visibleRelation, args
}

func (i Incarnation) zero() bool {
	return i.Table == "" && i.Version == 0 && i.DropGen == 0 && i.NsGen == [16]byte{}
}

func ScopeTableMismatch(want, got string) error {
	return conflictf("this request's row scope was resolved against table %q but the operation targets %q; re-read the table and retry", want, got)
}

func ScopeIncarnationChanged(nsName, table string) error {
	return conflictf("table %s.%s changed between the moment this request's row visibility was decided and its execution (a migration, or a drop and recreate); re-read the table and retry", nsName, table)
}

func IncarnationIsZero(i Incarnation) bool { return i.zero() }

func checkScopeIncarnation(ctx context.Context, tx rowQuerier, nsName, table string, want Incarnation) error {
	if want.zero() {
		return nil
	}
	if want.Table != table {
		return ScopeTableMismatch(want.Table, table)
	}
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return err
	}
	dropGen, err := tableGen(ctx, tx, table)
	if err != nil {
		return err
	}
	gen, err := readNSGen(ctx, tx)
	if err != nil {
		return err
	}
	got := Incarnation{NsGen: gen, Table: table, Version: int64(sc.Version), DropGen: dropGen}
	if got != want {
		return ScopeIncarnationChanged(nsName, table)
	}
	return nil
}

var ErrScopedKeyUpsertUnsupported = fmt.Errorf("%w: this request is scoped to your own rows, and upsert_by_key matches on the natural key across every row, so it could update a row you cannot see; insert instead, or ask for the read verb on the table, which lifts the scope", ErrInvalid)

func (s *Store) guardIncarnation(ctx context.Context, nsName, table string, want Incarnation) error {
	if want.zero() {
		return nil
	}
	n, err := s.ns(nsName)
	if err != nil {
		return err
	}
	return checkScopeIncarnation(ctx, n.ro, nsName, table, want)
}

var ErrScopedFeedUnsupported = fmt.Errorf("%w: this request is scoped to your own rows, and the change feed does not yet filter records by owner; ask for the read verb on the table, which lifts the scope", ErrInvalid)

var ErrScopedPlanUnsupported = fmt.Errorf("%w: this request is scoped to your own rows, and a migration plan reports table-wide information; ask for the read verb on the table, which lifts the scope", ErrInvalid)

var ErrScopedIdempotencyUnsupported = fmt.Errorf("%w: this request is scoped to your own rows, and idempotency keys are recorded per table rather than per owner, so replaying someone else's key would report their rows; retry without idempotency_key, or ask for the read verb on the table, which lifts the scope", ErrInvalid)
