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

var errScopedFilterUnsupported = fmt.Errorf("%w: this request is scoped to your own rows, and a filter expression is not yet accepted on a scoped call; select rows with the operation's structured arguments instead, or ask for the read verb on the table, which lifts the scope", ErrInvalid)

func (i Incarnation) zero() bool {
	return i.Table == "" && i.Version == 0 && i.DropGen == 0 && i.NsGen == [16]byte{}
}

func checkScopeIncarnation(ctx context.Context, tx rowQuerier, nsName, table string, want Incarnation) error {
	if want.zero() {
		return nil
	}
	if want.Table != table {
		return conflictf("this request's row scope was resolved against table %q but the operation targets %q; re-read the table and retry", want.Table, table)
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
		return conflictf("table %s.%s changed between the moment this request's row visibility was decided and its execution (a migration, or a drop and recreate); re-read the table and retry", nsName, table)
	}
	return nil
}

var errScopedKeyUpsertUnsupported = fmt.Errorf("%w: this request is scoped to your own rows, and upsert_by_key matches on the natural key across every row, so it could update a row you cannot see; insert instead, or ask for the read verb on the table, which lifts the scope", ErrInvalid)
