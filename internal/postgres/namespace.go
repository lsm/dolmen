package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) CreateNamespace(ctx context.Context, name string, parentGen [16]byte) error {
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
	if err := s.catalogLock(ctx, tx); err != nil {
		return err
	}
	if parentGen != [16]byte{} {
		pos := strings.LastIndexByte(name, '/')
		if pos < 0 {
			return fmt.Errorf("%w: root namespace has no parent", store.ErrInvalid)
		}
		parent, err := s.namespace(ctx, tx, name[:pos], namespaceKeyPinned)
		if err != nil {
			return err
		}
		if parent.generation != parentGen {
			return fmt.Errorf("%w: parent namespace was replaced", store.ErrNotFound)
		}
	}
	if _, err := s.namespace(ctx, tx, name, namespaceUnlocked); err == nil {
		return fmt.Errorf("%w: namespace %s %w", store.ErrInvalid, name, store.ErrExists)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var gen [16]byte
	for gen == [16]byte{} {
		if _, err := rand.Read(gen[:]); err != nil {
			return err
		}
	}
	physical := "dolmen_ns_" + hex.EncodeToString(gen[:])
	if _, err := tx.Exec(ctx, "CREATE SCHEMA "+ident(physical)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "REVOKE ALL ON SCHEMA "+ident(physical)+" FROM PUBLIC"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+s.relation("namespaces")+" (name, physical, generation) VALUES ($1, $2, $3)", name, physical, gen[:]); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.forgetQueryGrant(name)
	return nil
}

func (s *Store) NamespaceState(ctx context.Context, name string, auth []store.AuthBinding) ([16]byte, error) {
	done, err := s.begin(ctx)
	if err != nil {
		return [16]byte{}, err
	}
	defer done()
	if err := store.ValidateNamespace(name); err != nil {
		return [16]byte{}, err
	}
	if len(auth) != 0 {
		return [16]byte{}, derr.New(derr.Forbidden, "PostgreSQL authorization bindings are not implemented yet")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return [16]byte{}, err
	}
	defer rollback(tx)
	n, err := s.namespace(ctx, tx, name, namespaceUnlocked)
	return n.generation, err
}

func (s *Store) ListNamespaces(ctx context.Context, prefix string, auth []store.AuthBinding) ([]string, error) {
	done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if prefix != "" {
		if err := store.ValidateNamespace(prefix); err != nil {
			return nil, err
		}
	}
	if len(auth) != 0 {
		return nil, derr.New(derr.Forbidden, "PostgreSQL authorization bindings are not implemented yet")
	}
	rows, err := s.pool.Query(ctx, "SELECT name FROM "+s.relation("namespaces")+" WHERE $1 = '' OR name = $1 OR starts_with(name, $1 || '/')", prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]+".db" < out[j]+".db" })
	return out, rows.Err()
}

func (s *Store) DropNamespace(ctx context.Context, name string, expected [16]byte) error {
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
	if err := s.catalogLock(ctx, tx); err != nil {
		return err
	}
	n, err := s.namespace(ctx, tx, name, namespaceExclusive)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: namespace %s does not exist, so nothing was dropped; list_namespaces shows what is there", store.ErrNotFound, name)
		}
		return err
	}
	if expected != [16]byte{} && expected != n.generation {
		return fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, name)
	}
	var count int64
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+s.relation("namespaces")+" WHERE starts_with(name, $1 || '/')", name).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: namespace %s has %d descendant namespaces — drop the children first", store.ErrInvalid, name, count)
	}
	if _, err := tx.Exec(ctx, "DROP SCHEMA "+ident(n.physical)+" CASCADE"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM "+s.relation("namespaces")+" WHERE name = $1", name); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.forgetQueryGrant(name)
	return nil
}
