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

func rowCountTriggerDDL(table string, hasOwner bool) []string {
	insert, del := rowCountTriggers(table)
	bump := func(row, sign string) string {
		stmt := fmt.Sprintf(`INSERT INTO %s(table_name, scoped, owner, n) VALUES(%s, 0, '', %s) ON CONFLICT(table_name, scoped, owner) DO UPDATE SET n = n + (%s);`,
			rowCountsTable, sqlString(table), sign, sign)
		if hasOwner {
			owner := row + "." + q(schema.OwnerColumn)
			stmt += fmt.Sprintf(` INSERT INTO %s(table_name, scoped, owner, n) SELECT %s, 1, %s, %s WHERE %s IS NOT NULL ON CONFLICT(table_name, scoped, owner) DO UPDATE SET n = n + (%s);`,
				rowCountsTable, sqlString(table), owner, sign, owner, sign)
		}
		return stmt
	}
	return []string{
		fmt.Sprintf(`CREATE TRIGGER %s AFTER INSERT ON %s BEGIN %s END`, q(insert), q(table), bump("NEW", "1")),
		fmt.Sprintf(`CREATE TRIGGER %s AFTER DELETE ON %s BEGIN %s END`, q(del), q(table), bump("OLD", "-1")),
	}
}

func installRowCount(ctx context.Context, tx *sql.Tx, table string, hasOwner bool) error {
	insert, del := rowCountTriggers(table)
	stmts := []string{
		fmt.Sprintf(`DROP TRIGGER IF EXISTS %s`, q(insert)),
		fmt.Sprintf(`DROP TRIGGER IF EXISTS %s`, q(del)),
	}
	stmts = append(stmts, rowCountTriggerDDL(table, hasOwner)...)
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("install row count for %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+rowCountsTable+` WHERE table_name = ?`, table); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(table_name, scoped, owner, n) SELECT ?, 0, '', count(*) FROM %s`, rowCountsTable, q(table)), table); err != nil {
		return err
	}
	if hasOwner {
		owner := q(schema.OwnerColumn)
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(table_name, scoped, owner, n) SELECT ?, 1, %s, count(*) FROM %s WHERE %s IS NOT NULL GROUP BY %s`, rowCountsTable, owner, q(table), owner, owner), table); err != nil {
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
	scoped, owner := 0, ""
	if scope != nil {
		scoped, owner = 1, scope.Owner
	}
	var n int64
	err := db.QueryRowContext(ctx, `SELECT n FROM `+rowCountsTable+` WHERE table_name = ? AND scoped = ? AND owner = ?`, table, scoped, owner).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		if scope != nil {
			return 0, nil
		}
		return -1, nil
	}
	return n, err
}
