package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/store"
)

const catalogVersion = 1

func ident(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

func (s *Store) relation(name string) string { return ident(s.catalog, name) }

func (s *Store) catalogLock(ctx context.Context, tx pgx.Tx) error {
	sum := sha256.Sum256([]byte("dolmen/catalog/" + s.catalog))
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(sum[:8])))
	return err
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func (s *Store) bootstrap(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := s.catalogLock(ctx, tx); err != nil {
		return err
	}
	statements := []string{
		"CREATE SCHEMA IF NOT EXISTS " + ident(s.catalog),
		"REVOKE ALL ON SCHEMA " + ident(s.catalog) + " FROM PUBLIC",
		"CREATE TABLE IF NOT EXISTS " + s.relation("version") + " (singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton), version integer NOT NULL)",
		"INSERT INTO " + s.relation("version") + " (version) VALUES (1) ON CONFLICT DO NOTHING",
	}
	for _, stmt := range statements {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	var version int
	if err := tx.QueryRow(ctx, "SELECT version FROM "+s.relation("version")).Scan(&version); err != nil {
		return err
	}
	if version != catalogVersion {
		return fmt.Errorf("%w: PostgreSQL catalog version %d is unsupported; use a compatible dolmen release", store.ErrCatalogTooNew, version)
	}
	_, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+s.relation("namespaces")+` (
  name text COLLATE "C" PRIMARY KEY,
  physical text NOT NULL UNIQUE,
  generation bytea NOT NULL UNIQUE CHECK (octet_length(generation) = 16),
  next_change bigint NOT NULL DEFAULT 0 CHECK (next_change >= 0)
 )`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type namespace struct {
	name       string
	physical   string
	generation [16]byte
}

func (s *Store) namespace(ctx context.Context, tx pgx.Tx, name string, lock bool) (namespace, error) {
	stmt := "SELECT physical, generation FROM " + s.relation("namespaces") + " WHERE name = $1"
	if lock {
		stmt += " FOR UPDATE"
	}
	var n namespace
	var raw []byte
	n.name = name
	if err := tx.QueryRow(ctx, stmt, name).Scan(&n.physical, &raw); err != nil {
		if err == pgx.ErrNoRows {
			return n, fmt.Errorf("%w: namespace %s; use list_namespaces", store.ErrNotFound, name)
		}
		return n, err
	}
	if len(raw) != 16 {
		return n, fmt.Errorf("postgres: corrupt namespace generation")
	}
	copy(n.generation[:], raw)
	return n, nil
}

func (s *Store) write(ctx context.Context, name string, expected [16]byte, fn func(pgx.Tx, namespace) error) error {
	done, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := store.ValidateNamespace(name); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	n, err := s.namespace(ctx, tx, name, true)
	if err != nil {
		return err
	}
	if expected != [16]byte{} && expected != n.generation {
		return fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, name)
	}
	if err := fn(tx, n); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) reserveChanges(ctx context.Context, tx pgx.Tx, n namespace, count int64) (store.ChangeRange, error) {
	if count <= 0 {
		return store.ChangeRange{}, fmt.Errorf("%w: change count must be positive", store.ErrInvalid)
	}
	var last int64
	err := tx.QueryRow(ctx, "UPDATE "+s.relation("namespaces")+" SET next_change = next_change + $1 WHERE name = $2 AND generation = $3 RETURNING next_change", count, n.name, n.generation[:]).Scan(&last)
	if err != nil {
		return store.ChangeRange{}, err
	}
	return store.ChangeRange{First: last - count + 1, Last: last, Count: count}, nil
}
