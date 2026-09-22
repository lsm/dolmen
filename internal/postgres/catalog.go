package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/store"
)

const catalogVersion = 6

const minimumServerVersion = 160000

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

type serverVersionError struct {
	have string
	need int
}

func (e *serverVersionError) Error() string {
	return fmt.Sprintf("postgres: this engine needs PostgreSQL %d or newer and the server reports %s; a scoped filter that coerces text to a number calls pg_input_is_valid, which that server does not have", e.need, e.have)
}

func (e *serverVersionError) Unwrap() error { return store.ErrInvalid }

func (s *Store) requireServerVersion(ctx context.Context, tx pgx.Tx) error {
	var raw string
	if err := tx.QueryRow(ctx, "SHOW server_version_num").Scan(&raw); err != nil {
		return err
	}
	num, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: PostgreSQL reported an unreadable server_version_num %q", store.ErrInvalid, raw)
	}
	if num >= minimumServerVersion {
		return nil
	}
	shown := raw
	var reported string
	if err := tx.QueryRow(ctx, "SHOW server_version").Scan(&reported); err == nil {
		shown = reported
	}
	return &serverVersionError{have: shown, need: minimumServerVersion / 10000}
}

func (s *Store) bootstrap(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := s.requireServerVersion(ctx, tx); err != nil {
		return err
	}
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
	if version < 1 || version > catalogVersion {
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
	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+s.relation("tables")+` (
  namespace text NOT NULL REFERENCES `+s.relation("namespaces")+`(name) ON DELETE CASCADE,
  name text COLLATE "C" NOT NULL,
  physical text,
  schema_json text NOT NULL,
  columns_json text NOT NULL,
  drop_generation bigint NOT NULL DEFAULT 0 CHECK (drop_generation >= 0),
  active boolean NOT NULL DEFAULT true,
  PRIMARY KEY(namespace, name),
  UNIQUE(namespace, physical),
  CHECK ((active AND physical IS NOT NULL) OR (NOT active AND physical IS NULL))
 )`); err != nil {
		return err
	}
	for _, stmt := range []string{
		"CREATE TABLE IF NOT EXISTS " + s.relation("idempotency_owned") + ` (
 namespace text NOT NULL REFERENCES ` + s.relation("namespaces") + `(name) ON DELETE CASCADE,
 table_name text NOT NULL, drop_generation bigint NOT NULL, owner text NOT NULL, key text NOT NULL,
 payload_hash text NOT NULL, result_json text NOT NULL,
 PRIMARY KEY(namespace,table_name,drop_generation,owner,key))`,
		"CREATE TABLE IF NOT EXISTS " + s.relation("changes") + ` (
 namespace text NOT NULL REFERENCES ` + s.relation("namespaces") + `(name) ON DELETE CASCADE,
 position bigint NOT NULL, table_name text NOT NULL, drop_generation bigint NOT NULL,
 row_id bigint NOT NULL, kind text NOT NULL CHECK(kind IN ('insert','update','delete')),
 owner text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(namespace,position))`,
		"CREATE TABLE IF NOT EXISTS " + s.relation("migrations") + ` (
 namespace text NOT NULL REFERENCES ` + s.relation("namespaces") + `(name) ON DELETE CASCADE,
 table_name text NOT NULL, drop_generation bigint NOT NULL, id bigint NOT NULL,
 from_version integer NOT NULL, to_version integer NOT NULL, changes_json text NOT NULL,
 at text NOT NULL DEFAULT to_char(statement_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
 PRIMARY KEY(namespace,table_name,drop_generation,id))`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	if err := s.retireOwnerlessIdempotency(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+s.relation("cursors")+` (
 namespace text NOT NULL REFERENCES `+s.relation("namespaces")+`(name) ON DELETE CASCADE,
 token text NOT NULL, position bigint NOT NULL, chain_origin bigint NOT NULL,
 chain_start timestamptz NOT NULL, issued_at timestamptz NOT NULL,
 table_name text NOT NULL, drop_generation bigint NOT NULL,
 PRIMARY KEY(namespace,token))`); err != nil {
		return err
	}
	for _, stmt := range []string{
		"ALTER TABLE " + s.relation("changes") + " ADD COLUMN IF NOT EXISTS owner text",
		"CREATE INDEX IF NOT EXISTS changes_feed ON " + s.relation("changes") + " (namespace,table_name,drop_generation,position)",
		"CREATE INDEX IF NOT EXISTS changes_owner_feed ON " + s.relation("changes") + " (namespace,table_name,drop_generation,owner,position)",
		"CREATE INDEX IF NOT EXISTS cursors_origin ON " + s.relation("cursors") + " (namespace,chain_origin)",
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "UPDATE "+s.relation("version")+" SET version = $1", catalogVersion); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) retireOwnerlessIdempotency(ctx context.Context, tx pgx.Tx) error {
	var legacy *string
	if err := tx.QueryRow(ctx, "SELECT to_regclass($1)::text", s.relation("idempotency")).Scan(&legacy); err != nil {
		return err
	}
	if legacy == nil {
		return nil
	}
	for _, stmt := range []string{
		"INSERT INTO " + s.relation("idempotency_owned") +
			" (namespace,table_name,drop_generation,owner,key,payload_hash,result_json)" +
			" SELECT namespace,table_name,drop_generation,'',key,payload_hash,result_json FROM " +
			s.relation("idempotency") + " ON CONFLICT DO NOTHING",
		"DROP TABLE " + s.relation("idempotency"),
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("give idempotency records an owner domain: %w", err)
		}
	}
	return nil
}

type namespace struct {
	name       string
	physical   string
	generation [16]byte
}

type namespaceLock int

const (
	namespaceUnlocked namespaceLock = iota
	namespaceKeyPinned
	namespaceWriteSerialized
	namespaceExclusive
)

func (l namespaceLock) clause() string {
	switch l {
	case namespaceKeyPinned:
		return " FOR KEY SHARE"
	case namespaceWriteSerialized:
		return " FOR NO KEY UPDATE"
	case namespaceExclusive:
		return " FOR UPDATE"
	}
	return ""
}

func (s *Store) namespace(ctx context.Context, tx pgx.Tx, name string, lock namespaceLock) (namespace, error) {
	stmt := "SELECT physical, generation FROM " + s.relation("namespaces") + " WHERE name = $1" + lock.clause()
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

func (s *Store) writeUnlocked(ctx context.Context, name string, expected [16]byte, fn func(pgx.Tx, namespace) error) error {
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
	n, err := s.namespace(ctx, tx, name, namespaceKeyPinned)
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
	n, err := s.namespace(ctx, tx, name, namespaceWriteSerialized)
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
