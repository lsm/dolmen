package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/lsm/dolmen/internal/constraints"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type constraintTable struct {
	physical string
	columns  map[string]string
}

func (s *Store) constrained(ctx context.Context, table string, incoming bool, fn func(pgx.Tx, namespace) error) func(pgx.Tx, namespace) error {
	return func(tx pgx.Tx, n namespace) error {
		var before int64
		if n.reserved != nil {
			before = *n.reserved
		}
		if err := fn(tx, n); err != nil {
			return err
		}
		if n.reserved != nil && *n.reserved == before {
			return nil
		}
		return s.checkConstraints(ctx, tx, n, table, incoming)
	}
}

func (s *Store) checkConstraints(ctx context.Context, tx pgx.Tx, n namespace, table string, incoming bool) error {
	var raw string
	err := tx.QueryRow(ctx, "SELECT schema_json FROM "+s.relation("tables")+" WHERE namespace=$1 AND name=$2 AND active", n.name, table).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	sc, err := decodeConstraintSchema(raw)
	if err != nil {
		return err
	}
	if len(sc.Constraints) == 0 && !(incoming && sc.AcceptReferences) {
		return nil
	}
	tables, err := s.constraintSnapshot(ctx, tx, n, table, incoming)
	if err != nil {
		return err
	}
	err = constraints.CheckCatalog(ctx, tables)
	if err == nil || errors.Is(err, derr.ErrConflict) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", store.ErrInvalid, err)
}

func decodeConstraintSchema(raw string) (*schema.TableSchema, error) {
	var sc schema.TableSchema
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("postgres: corrupt table schema: %w", err)
	}
	return &sc, nil
}

func (s *Store) constraintSnapshot(ctx context.Context, tx pgx.Tx, n namespace, changed string, incoming bool) (map[string]constraints.Table, error) {
	rows, err := tx.Query(ctx, "SELECT name, physical, schema_json, columns_json FROM "+s.relation("tables")+" WHERE namespace=$1 AND active ORDER BY name", n.name)
	if err != nil {
		return nil, err
	}
	tables := map[string]constraints.Table{}
	layout := map[string]constraintTable{}
	for rows.Next() {
		var name, physical, raw, columns string
		if err := rows.Scan(&name, &physical, &raw, &columns); err != nil {
			rows.Close()
			return nil, err
		}
		sc, err := decodeConstraintSchema(raw)
		if err != nil {
			rows.Close()
			return nil, err
		}
		t := constraintTable{physical: physical}
		if err := json.Unmarshal([]byte(columns), &t.columns); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: corrupt column mapping: %w", err)
		}
		tables[name] = constraints.Table{Schema: sc}
		layout[name] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tables = constraints.SelectChecks(tables, changed, incoming)
	needed := map[string]bool{}
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
	for name := range needed {
		t, ok := tables[name]
		if !ok {
			continue
		}
		t.Rows, err = s.constraintRows(ctx, tx, n, layout[name], t.Schema)
		if err != nil {
			return nil, err
		}
		tables[name] = t
	}
	return tables, nil
}

func (s *Store) constraintRows(ctx context.Context, tx pgx.Tx, n namespace, t constraintTable, sc *schema.TableSchema) (map[int64]constraints.Row, error) {
	owner := "NULL::text"
	if sc.HasOwner {
		owner = ident(schema.OwnerColumn)
	}
	columns := []string{"id", owner}
	var fields []schema.Field
	for _, f := range sc.Fields {
		switch f.Type {
		case schema.String, schema.Text, schema.Timestamp:
			columns = append(columns, ident(t.columns[f.Name]))
		case schema.Number:
			columns = append(columns, ident(t.columns[f.Name])+"::text")
		case schema.Boolean:
			columns = append(columns, ident(t.columns[f.Name]))
		default:
			continue
		}
		fields = append(fields, f)
	}
	rows, err := tx.Query(ctx, "SELECT "+strings.Join(columns, ",")+" FROM "+ident(n.physical, t.physical))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int64]constraints.Row{}
	for rows.Next() {
		var id int64
		var ownerValue *string
		values := make([]any, len(fields))
		dest := []any{&id, &ownerValue}
		for i, f := range fields {
			switch f.Type {
			case schema.Boolean:
				values[i] = new(*bool)
			default:
				values[i] = new(*string)
			}
			dest = append(dest, values[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := constraints.Row{Owner: ownerValue, Values: make(map[string]any, len(fields))}
		for i, f := range fields {
			switch v := values[i].(type) {
			case **bool:
				if *v != nil {
					row.Values[f.Name] = **v
				}
			case **string:
				if *v == nil {
					continue
				}
				if f.Type == schema.Number {
					row.Values[f.Name] = json.Number(**v)
				} else {
					row.Values[f.Name] = **v
				}
			}
		}
		result[id] = row
	}
	return result, rows.Err()
}

func (s *Store) refuseReferencedDrop(ctx context.Context, tx pgx.Tx, n namespace, table string) error {
	rows, err := tx.Query(ctx, "SELECT name, schema_json FROM "+s.relation("tables")+" WHERE namespace=$1 AND active AND name <> $2 ORDER BY name", n.name, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return err
		}
		sc, err := decodeConstraintSchema(raw)
		if err != nil {
			return err
		}
		for _, c := range sc.Constraints {
			if c.Kind == schema.ConstraintForeignKey && c.References != nil && c.References.Table == table {
				return fmt.Errorf("%w: table %s is referenced by a foreign key declared on table %s; drop that constraint first", store.ErrInvalid, table, name)
			}
		}
	}
	return rows.Err()
}
