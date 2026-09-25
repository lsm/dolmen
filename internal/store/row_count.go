package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

const rowCountsTable = "_dolmen_row_counts"

func rowCountTriggers(table string) (insert, del string) {
	return "_dolmen_count_insert_" + table, "_dolmen_count_delete_" + table
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func rowCountTriggerDDL(table string, gen int64, hasOwner bool) []string {
	insert, del := rowCountTriggers(table)
	tracks := 0
	if hasOwner {
		tracks = 1
	}
	bump := func(row, sign string) string {
		stmt := fmt.Sprintf(`INSERT INTO %s(table_name, scoped, owner, drop_gen, tracks_owners, n) VALUES(%s, 0, '', %d, %d, %s) ON CONFLICT(table_name, scoped, owner) DO UPDATE SET n = n + (%s);`,
			rowCountsTable, sqlString(table), gen, tracks, sign, sign)
		if hasOwner {
			owner := row + "." + q(schema.OwnerColumn)
			stmt += fmt.Sprintf(` INSERT INTO %s(table_name, scoped, owner, drop_gen, tracks_owners, n) SELECT %s, 1, %s, %d, 1, %s WHERE %s IS NOT NULL ON CONFLICT(table_name, scoped, owner) DO UPDATE SET n = n + (%s);`,
				rowCountsTable, sqlString(table), owner, gen, sign, owner, sign)
		}
		return stmt
	}
	return []string{
		fmt.Sprintf(`CREATE TRIGGER %s AFTER INSERT ON %s BEGIN %s END`, q(insert), q(table), bump("NEW", "1")),
		fmt.Sprintf(`CREATE TRIGGER %s AFTER DELETE ON %s BEGIN %s END`, q(del), q(table), bump("OLD", "-1")),
	}
}

func installRowCount(ctx context.Context, tx *sql.Tx, table string, hasOwner bool) error {
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		return err
	}
	insert, del := rowCountTriggers(table)
	stmts := []string{
		fmt.Sprintf(`DROP TRIGGER IF EXISTS %s`, q(insert)),
		fmt.Sprintf(`DROP TRIGGER IF EXISTS %s`, q(del)),
	}
	stmts = append(stmts, rowCountTriggerDDL(table, gen, hasOwner)...)
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("install row count for %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+rowCountsTable+` WHERE table_name = ?`, table); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(table_name, scoped, owner, drop_gen, tracks_owners, n) SELECT ?, 0, '', ?, ?, count(*) FROM %s`, rowCountsTable, q(table)), table, gen, hasOwner); err != nil {
		return err
	}
	if hasOwner {
		owner := q(schema.OwnerColumn)
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(table_name, scoped, owner, drop_gen, tracks_owners, n) SELECT ?, 1, %s, ?, 1, count(*) FROM %s WHERE %s IS NOT NULL GROUP BY %s`, rowCountsTable, owner, q(table), owner, owner), table, gen); err != nil {
			return err
		}
	}
	return nil
}

func ensureRowCounts(ctx context.Context, rw *sql.DB, nsName string) error {
	rows, err := rw.QueryContext(ctx, `SELECT t.name FROM _dolmen_tables t
		WHERE EXISTS(SELECT 1 FROM sqlite_master m WHERE m.type = 'table' AND m.name = t.name)
		AND NOT EXISTS(SELECT 1 FROM sqlite_master m WHERE m.type = 'trigger' AND m.name = '_dolmen_count_insert_' || t.name)
		ORDER BY t.name`)
	if err != nil {
		return err
	}
	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		missing = append(missing, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, table := range missing {
		tx, err := rw.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		sc, err := loadSchema(ctx, tx, nsName, table)
		if err == nil {
			err = installRowCount(ctx, tx, table, sc.HasOwner)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return nil
}

func readRowCount(ctx context.Context, db rowQuerier, table string, scope *RowScope) (int64, error) {
	var total, countedGen int64
	var tracksOwners bool
	err := db.QueryRowContext(ctx, `SELECT n, drop_gen, tracks_owners FROM `+rowCountsTable+` WHERE table_name = ? AND scoped = 0 AND owner = ''`, table).Scan(&total, &countedGen, &tracksOwners)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	gen, err := tableGen(ctx, db, table)
	if err != nil {
		return 0, err
	}
	if gen != countedGen {
		return -1, nil
	}
	if scope == nil {
		return total, nil
	}
	if !tracksOwners {
		return -1, nil
	}
	var n int64
	err = db.QueryRowContext(ctx, `SELECT n FROM `+rowCountsTable+` WHERE table_name = ? AND scoped = 1 AND owner = ?`, table, scope.Owner).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
