package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

type catalogDrift struct {
	table, problem string
}

func reconcileCatalog(ctx context.Context, db *sql.DB, nsName string) ([]catalogDrift, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM _dolmen_tables ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var drift []catalogDrift
	for _, table := range tables {
		sc, err := loadSchema(ctx, db, nsName, table)
		if err != nil {
			drift = append(drift, catalogDrift{table, err.Error()})
			continue
		}
		cols, err := physicalColumns(ctx, db, table)
		if err != nil {
			return nil, err
		}
		if len(cols) == 0 {
			drift = append(drift, catalogDrift{table, "the catalog lists it but the table itself is missing"})
			continue
		}
		for _, f := range sc.Fields {
			if !cols[f.Name] {
				drift = append(drift, catalogDrift{table, fmt.Sprintf("field %s has no column", f.Name)})
			}
		}
		if sc.VectorizeField() != nil && !cols["_embedding"] {
			drift = append(drift, catalogDrift{table, "the vectorized field has no _embedding column"})
		}
		if len(ftsFields(sc.Fields)) > 0 {
			var n int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, ftsTable(table)).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				drift = append(drift, catalogDrift{table, "its full-text fields have no full-text index"})
			}
		}
	}
	return drift, nil
}

func physicalColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

func warnCatalogDrift(ctx context.Context, db *sql.DB, nsName string) {
	drift, err := reconcileCatalog(ctx, db, nsName)
	if err != nil {
		slog.Warn("could not check the namespace catalog against its tables", "namespace", nsName, "err", err)
		return
	}
	for _, d := range drift {
		slog.Warn("namespace catalog does not match the file; operations on this table may fail. Restore the namespace from a backup, or drop and recreate the table", "namespace", nsName, "table", d.table, "problem", d.problem)
	}
}
