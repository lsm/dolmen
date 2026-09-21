package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type tableState struct {
	schema      *schema.TableSchema
	physical    string
	columns     map[string]string
	incarnation store.Incarnation
}

func (s *Store) loadTable(ctx context.Context, tx pgx.Tx, n namespace, table string) (tableState, error) {
	var result tableState
	var raw, columns string
	var generation int64
	err := tx.QueryRow(ctx, "SELECT physical, schema_json, columns_json, drop_generation FROM "+s.relation("tables")+" WHERE namespace=$1 AND name=$2 AND active", n.name, table).Scan(&result.physical, &raw, &columns, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, fmt.Errorf("%w: table %s.%s", store.ErrNotFound, n.name, table)
	}
	if err != nil {
		return result, err
	}
	if err := decodeTableState(&result, n, table, raw, columns, generation); err != nil {
		return result, err
	}
	return result, nil
}

func decodeTableState(result *tableState, n namespace, table, raw, columns string, generation int64) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&result.schema); err != nil {
		return fmt.Errorf("postgres: corrupt table schema: %w", err)
	}
	if result.schema == nil {
		return fmt.Errorf("postgres: missing table schema")
	}
	if err := json.Unmarshal([]byte(columns), &result.columns); err != nil {
		return fmt.Errorf("postgres: corrupt column mapping: %w", err)
	}
	result.incarnation = store.Incarnation{NsGen: n.generation, Table: table, Version: int64(result.schema.Version), DropGen: generation}
	return nil
}

func (s *Store) read(ctx context.Context, name string, fn func(pgx.Tx, namespace) error) error {
	return s.readMode(ctx, name, false, fn)
}

func (s *Store) readOnly(ctx context.Context, name string, fn func(pgx.Tx, namespace) error) error {
	return s.readMode(ctx, name, true, fn)
}

func (s *Store) readMode(ctx context.Context, name string, readOnly bool, fn func(pgx.Tx, namespace) error) error {
	done, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := store.ValidateNamespace(name); err != nil {
		return err
	}
	options := pgx.TxOptions{IsoLevel: pgx.ReadCommitted}
	if readOnly {
		options.AccessMode = pgx.ReadOnly
	}
	tx, err := s.pool.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var n namespace
	var gen []byte
	n.name = name
	query := "SELECT physical,generation FROM " + s.relation("namespaces") + " WHERE name=$1"
	if !readOnly {
		query += " FOR SHARE"
	}
	err = tx.QueryRow(ctx, query, name).Scan(&n.physical, &gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: namespace %s; use list_namespaces", store.ErrNotFound, name)
	}
	if err != nil {
		return err
	}
	if len(gen) != 16 {
		return fmt.Errorf("postgres: corrupt namespace generation")
	}
	copy(n.generation[:], gen)
	return fn(tx, n)
}

func (s *Store) CreateTable(ctx context.Context, ns, table string, fields []schema.Field, opts store.TableOpts, expected [16]byte) (*schema.TableSchema, error) {
	fields, err := store.ValidateTableDefinition(table, fields)
	if err != nil {
		return nil, err
	}
	if err := schema.ValidateRowAccess(opts.RowAccess); err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	if opts.RowAccess != "" {
		if err := store.ValidateOwnerCollision(fields); err != nil {
			return nil, err
		}
	}
	sc := &schema.TableSchema{Namespace: ns, Name: table, Version: 1, Fields: fields,
		RowAccess: opts.RowAccess, HasOwner: opts.RowAccess != ""}
	err = s.write(ctx, ns, expected, func(tx pgx.Tx, n namespace) error {
		if _, err := s.loadTable(ctx, tx, n, table); err == nil {
			return fmt.Errorf("%w: table %s.%s already exists", store.ErrInvalid, ns, table)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		physical, err := physicalTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		columns, err := physicalColumns(fields)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, tableDDL(n.physical, physical, fields, columns, sc.HasOwner)); err != nil {
			return err
		}
		if len(fulltextFields(fields)) > 0 {
			indexDDL, err := ftsIndexDDL(ctx, tx, n, physical)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, indexDDL); err != nil {
				return err
			}
		}
		if err := s.grantQueryTable(ctx, tx, n, physical, fields, columns, sc.HasOwner); err != nil {
			return err
		}
		raw, err := json.Marshal(sc)
		if err != nil {
			return err
		}
		columnJSON, err := json.Marshal(columns)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO "+s.relation("tables")+`(namespace,name,physical,schema_json,columns_json) VALUES($1,$2,$3,$4,$5)
  ON CONFLICT(namespace,name) DO UPDATE SET physical=EXCLUDED.physical,schema_json=EXCLUDED.schema_json,columns_json=EXCLUDED.columns_json,active=true`, ns, table, physical, string(raw), string(columnJSON))
		return err
	})
	if err != nil {
		return nil, err
	}
	return sc, nil
}

func columnType(f schema.Field) string {
	switch f.Type {
	case schema.Number:
		return "numeric"
	case schema.Boolean:
		return "boolean"
	case schema.Vector:
		return "bytea"
	}
	return `text COLLATE "C"`
}

func tableDDL(ns, table string, fields []schema.Field, columns map[string]string, hasOwner bool) string {
	parts := []string{"id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY", `created_at text NOT NULL DEFAULT to_char(statement_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`}
	vectorized := false
	for _, f := range fields {
		col := ident(columns[f.Name]) + " " + columnType(f)
		if f.Required {
			col += " NOT NULL"
		}
		parts = append(parts, col)
		vectorized = vectorized || f.Vectorize
	}
	if vectorized {
		parts = append(parts, `"_embedding" bytea`)
	}
	if hasOwner {
		parts = append(parts, ident(schema.OwnerColumn)+` text COLLATE "C"`)
	}
	if fts := ftsColumnDDL(fields, columns); fts != "" {
		parts = append(parts, fts)
	}
	return "CREATE TABLE " + ident(ns, table) + " (" + strings.Join(parts, ",") + ")"
}

func (s *Store) TableState(ctx context.Context, ns, table string, auth []store.AuthBinding) (*schema.TableSchema, store.Incarnation, error) {
	if len(auth) != 0 {
		return nil, store.Incarnation{}, derr.New(derr.Forbidden, "PostgreSQL authorization bindings are not implemented yet")
	}
	var result tableState
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		var err error
		result, err = s.loadTable(ctx, tx, n, table)
		return err
	})
	return result.schema, result.incarnation, err
}

func (s *Store) ListTables(ctx context.Context, ns string, auth []store.AuthBinding) ([]string, error) {
	if len(auth) != 0 {
		return nil, derr.New(derr.Forbidden, "PostgreSQL authorization bindings are not implemented yet")
	}
	out := []string{}
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		rows, err := tx.Query(ctx, "SELECT name FROM "+s.relation("tables")+" WHERE namespace=$1 AND active ORDER BY name", ns)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			out = append(out, name)
		}
		return rows.Err()
	})
	return out, err
}

func checkIncarnation(ns string, current, expected store.Incarnation) error {
	boundLifetime := expected.NsGen != [16]byte{} || expected.Table != "" || expected.DropGen != 0
	if expected.NsGen != [16]byte{} && expected.NsGen != current.NsGen || expected.Table != "" && expected.Table != current.Table || boundLifetime && expected.DropGen != current.DropGen {
		return fmt.Errorf("%w: table %s.%s was replaced; describe the current table", store.ErrNotFound, ns, current.Table)
	}
	if expected.Version != 0 && expected.Version != current.Version {
		return &store.VersionConflictError{Namespace: ns, Table: current.Table, ExpectedVersion: int(expected.Version), CurrentVersion: int(current.Version)}
	}
	return nil
}

func (s *Store) DescribeTable(ctx context.Context, ns, table string, scope *store.RowScope, expected store.Incarnation) (*schema.TableSchema, int64, error) {
	var result tableState
	var count int64
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		var err error
		result, err = s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := s.guardScope(ctx, tx, n, table, result, expected); err != nil {
			return err
		}
		if err := scopeUsable(scope, result.schema); err != nil {
			return err
		}
		if scope != nil && scope.Empty {
			count = 0
			return nil
		}
		stmt := "SELECT count(*) FROM " + ident(n.physical, result.physical)
		clause, args := scopePredicate(scope, "", 1)
		if clause != "" {
			stmt += " WHERE " + clause
		}
		return tx.QueryRow(ctx, stmt, args...).Scan(&count)
	})
	return result.schema, count, err
}

func (s *Store) DropTable(ctx context.Context, ns, table string, expected store.Incarnation) error {
	return s.write(ctx, ns, expected.NsGen, func(tx pgx.Tx, n namespace) error {
		current, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := checkIncarnation(ns, current.incarnation, expected); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DROP TABLE "+ident(n.physical, current.physical)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM "+s.relation("migrations")+" WHERE namespace=$1 AND table_name=$2", ns, table); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE "+s.relation("tables")+" SET active=false, physical=NULL, drop_generation=drop_generation+1 WHERE namespace=$1 AND name=$2", ns, table)
		return err
	})
}
