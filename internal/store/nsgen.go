package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
)

const nsGenKey = "nsgen"

func ensureNSGen(ctx context.Context, rw *sql.DB) error {

	var zero, gen [16]byte
	for gen == zero {
		if _, err := rand.Read(gen[:]); err != nil {
			return fmt.Errorf("mint nsgen: %w", err)
		}
	}
	tx, err := rw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _dolmen_meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`,
		nsGenKey, gen[:]); err != nil {
		return err
	}
	var stored []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM _dolmen_meta WHERE key = ?`, nsGenKey).Scan(&stored); err != nil {
		return err
	}
	if len(stored) != len(gen) {
		return fmt.Errorf("corrupt nsgen: %d bytes, want %d", len(stored), len(gen))
	}
	return tx.Commit()
}

func readNSGen(ctx context.Context, db rowQuerier) ([16]byte, error) {
	var raw []byte
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM _dolmen_meta WHERE key = ?`, nsGenKey).Scan(&raw); err != nil {
		return [16]byte{}, fmt.Errorf("read nsgen: %w", err)
	}
	if len(raw) != 16 {
		return [16]byte{}, fmt.Errorf("corrupt nsgen: %d bytes, want 16", len(raw))
	}
	var gen [16]byte
	copy(gen[:], raw)
	return gen, nil
}

func (s *Store) NamespaceState(ctx context.Context, nsName string, auth []AuthBinding) ([16]byte, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return [16]byte{}, err
	}
	defer n.unpin()
	return readNSGen(ctx, n.ro)
}
