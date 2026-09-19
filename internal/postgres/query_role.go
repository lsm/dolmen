package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) grantQueryTable(ctx context.Context, tx pgx.Tx, n namespace, physical string, fields []schema.Field, columns map[string]string) error {
	if s.queryRole == "" {
		return nil
	}
	cols := []string{ident("id"), ident("created_at")}
	for _, field := range fields {
		cols = append(cols, ident(columns[field.Name]))
	}
	_, err := tx.Exec(ctx, "GRANT SELECT ("+strings.Join(cols, ",")+") ON "+ident(n.physical, physical)+" TO "+ident(s.queryRole))
	return err
}

func (s *Store) queryTables(ctx context.Context, tx pgx.Tx, n namespace) (map[string]tableState, error) {
	rows, err := tx.Query(ctx, "SELECT name FROM "+s.relation("tables")+" WHERE namespace=$1 AND active", n.name)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]tableState{}
	for _, name := range names {
		state, err := s.loadTable(ctx, tx, n, name)
		if err != nil {
			return nil, err
		}
		out[name] = state
	}
	return out, nil
}

func (s *Store) ensureQueryRole(ctx context.Context, ns string, expected [16]byte) ([16]byte, error) {
	if s.queryRole == "" {
		return [16]byte{}, fmt.Errorf("%w: PostgreSQL caller SQL requires a pre-provisioned NOLOGIN query role", store.ErrInvalid)
	}
	var generation [16]byte
	err := s.write(ctx, ns, expected, func(tx pgx.Tx, n namespace) error {
		generation = n.generation
		var login, super, createDB, createRole, replicate, bypass bool
		err := tx.QueryRow(ctx, "SELECT rolcanlogin,rolsuper,rolcreatedb,rolcreaterole,rolreplication,rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=$1", s.queryRole).Scan(&login, &super, &createDB, &createRole, &replicate, &bypass)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: PostgreSQL query role %q does not exist", store.ErrInvalid, s.queryRole)
		}
		if err != nil {
			return err
		}
		if login || super || createDB || createRole || replicate || bypass {
			return fmt.Errorf("%w: PostgreSQL query role %q must be NOLOGIN, NOSUPERUSER, NOCREATEDB, NOCREATEROLE, NOREPLICATION, and NOBYPASSRLS", store.ErrInvalid, s.queryRole)
		}
		var canSet bool
		if err := tx.QueryRow(ctx, "SELECT pg_catalog.pg_has_role(current_user,$1,'SET')", s.queryRole).Scan(&canSet); err != nil {
			return err
		}
		if !canSet {
			return fmt.Errorf("%w: PostgreSQL backend account needs SET membership in query role %q", store.ErrInvalid, s.queryRole)
		}
		if _, err := tx.Exec(ctx, "GRANT USAGE ON SCHEMA "+ident(n.physical)+" TO "+ident(s.queryRole)); err != nil {
			return err
		}
		tables, err := s.queryTables(ctx, tx, n)
		if err != nil {
			return err
		}
		for _, table := range tables {
			if err := s.grantQueryTable(ctx, tx, n, table.physical, table.schema.Fields, table.columns); err != nil {
				return err
			}
		}
		return nil
	})
	return generation, err
}

func enterQueryRole(ctx context.Context, tx pgx.Tx, role string) error {
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '30s'"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "SET LOCAL ROLE "+ident(role))
	return err
}
