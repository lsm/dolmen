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
	rows, err := tx.Query(ctx, "SELECT name, physical, schema_json, columns_json, drop_generation FROM "+s.relation("tables")+" WHERE namespace=$1 AND active", n.name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]tableState{}
	for rows.Next() {
		var name, raw, columns string
		var state tableState
		var generation int64
		if err := rows.Scan(&name, &state.physical, &raw, &columns, &generation); err != nil {
			return nil, err
		}
		if err := decodeTableState(&state, n, name, raw, columns, generation); err != nil {
			return nil, err
		}
		out[name] = state
	}
	return out, rows.Err()
}

func (s *Store) queryGrant(ns string) ([16]byte, bool) {
	s.queryGrantMu.Lock()
	defer s.queryGrantMu.Unlock()
	generation, ok := s.queryGrants[ns]
	return generation, ok
}

func (s *Store) rememberQueryGrant(ns string, generation [16]byte) {
	s.queryGrantMu.Lock()
	defer s.queryGrantMu.Unlock()
	if s.queryGrants == nil {
		s.queryGrants = map[string][16]byte{}
	}
	s.queryGrants[ns] = generation
}

func (s *Store) forgetQueryGrant(ns string) {
	s.queryGrantMu.Lock()
	defer s.queryGrantMu.Unlock()
	delete(s.queryGrants, ns)
}

func (s *Store) forgetQueryGrantGeneration(ns string, generation [16]byte) {
	s.queryGrantMu.Lock()
	defer s.queryGrantMu.Unlock()
	if s.queryGrants[ns] == generation {
		delete(s.queryGrants, ns)
	}
}

func (s *Store) ensureQueryRole(ctx context.Context, ns string, expected [16]byte) ([16]byte, error) {
	if s.queryRole == "" {
		return [16]byte{}, fmt.Errorf("%w: PostgreSQL caller SQL requires a pre-provisioned NOLOGIN query role", store.ErrInvalid)
	}
	if generation, ok := s.queryGrant(ns); ok {
		if expected != [16]byte{} && expected != generation {
			return [16]byte{}, fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, ns)
		}
		return generation, nil
	}
	var generation [16]byte
	err := s.write(ctx, ns, expected, func(tx pgx.Tx, n namespace) error {
		generation = n.generation
		if cached, ok := s.queryGrant(ns); ok && cached == generation {
			return nil
		}
		var login, inherit, super, createDB, createRole, replicate, bypass bool
		err := tx.QueryRow(ctx, "SELECT rolcanlogin,rolinherit,rolsuper,rolcreatedb,rolcreaterole,rolreplication,rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=$1", s.queryRole).Scan(&login, &inherit, &super, &createDB, &createRole, &replicate, &bypass)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: PostgreSQL query role %q does not exist", store.ErrInvalid, s.queryRole)
		}
		if err != nil {
			return err
		}
		if login || inherit || super || createDB || createRole || replicate || bypass {
			return fmt.Errorf("%w: PostgreSQL query role %q must be NOLOGIN, NOINHERIT, NOSUPERUSER, NOCREATEDB, NOCREATEROLE, NOREPLICATION, and NOBYPASSRLS", store.ErrInvalid, s.queryRole)
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
	if err == nil {
		s.rememberQueryGrant(ns, generation)
	}
	return generation, err
}

func enterQueryRole(ctx context.Context, tx pgx.Tx, role string) error {
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '30s'"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "SET LOCAL ROLE "+ident(role))
	return err
}
