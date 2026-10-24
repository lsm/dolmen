package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/constraints"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func sqliteConstraintSnapshot(ctx context.Context, tx *sql.Tx, changed string, incoming bool) (map[string]constraints.Table, error) {
	registry, err := tx.QueryContext(ctx, `SELECT name,schema_json FROM _dolmen_tables ORDER BY name`)
	if err != nil {
		return nil, err
	}
	tables := make(map[string]constraints.Table)
	for registry.Next() {
		var name, raw string
		if err := registry.Scan(&name, &raw); err != nil {
			registry.Close()
			return nil, err
		}
		var sc schema.TableSchema
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&sc); err != nil {
			registry.Close()
			return nil, fmt.Errorf("%w: invalid schema in constraint snapshot", ErrCatalogCorrupt)
		}
		tables[name] = constraints.Table{Schema: &sc}

	}
	registry.Close()
	if err := registry.Err(); err != nil {
		return nil, err
	}
	tables = constraints.SelectChecks(tables, changed, incoming)
	needed := make(map[string]bool)
	for name, t := range tables {
		if len(t.Schema.Constraints) > 0 {
			needed[name] = true
		}
		for _, c := range t.Schema.Constraints {
			if c.Kind == schema.ConstraintForeignKey && c.References != nil {
				needed[c.References.Table] = true
			}
		}
	}
	for name, t := range tables {
		if !needed[name] {
			continue
		}
		t.Rows, err = sqliteConstraintRows(ctx, tx, name, t.Schema)
		if err != nil {
			return nil, err
		}
		tables[name] = t
	}
	return tables, nil
}

func sqliteConstraintRows(ctx context.Context, tx *sql.Tx, name string, sc *schema.TableSchema) (map[int64]constraints.Row, error) {
	columns := []string{q("id"), changeOwnerColumn(sc)}
	var fields []schema.Field
	for _, f := range sc.Fields {
		switch f.Type {
		case schema.String, schema.Text, schema.Number, schema.Boolean, schema.Timestamp:
			columns = append(columns, q(f.Name))
			fields = append(fields, f)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+strings.Join(columns, ",")+` FROM `+q(name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]constraints.Row)
	for rows.Next() {
		var id int64
		var owner sql.NullString
		values := make([]any, len(fields))
		dest := []any{&id, &owner}
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := constraints.Row{Values: make(map[string]any, len(fields))}
		if owner.Valid {
			principal := owner.String
			row.Owner = &principal
		}
		for i, f := range fields {
			row.Values[f.Name] = values[i]
		}
		result[id] = row
	}
	return result, rows.Err()
}

func checkSQLiteConstraints(ctx context.Context, tx *sql.Tx, sc *schema.TableSchema, incoming bool) error {
	if len(sc.Constraints) == 0 && !(incoming && sc.AcceptReferences) {
		return nil
	}
	tables, err := sqliteConstraintSnapshot(ctx, tx, sc.Name, incoming)
	if err != nil {
		return err
	}
	err = constraints.CheckCatalog(ctx, tables)
	if err == nil || errors.Is(err, derr.ErrConflict) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrInvalid, err)
}
