package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
)

// nsGenKey is the single _dolmen_meta row holding the namespace's creation
// id (§3.4).
const nsGenKey = "nsgen"

// ensureNSGen materializes the namespace's creation id in its registry:
// 16 crypto-random bytes, minted when the row is absent and left untouched
// when it exists. The INSERT-then-SELECT pair — not check-then-insert — is
// what makes a concurrent first open safe: another process sharing the data
// directory can initialize the same fresh file between this process's
// statements, SQLite's write lock serializes the two INSERTs, ON CONFLICT
// silently drops the loser's candidate, and the SELECT returns the winner's
// bytes to both. In-process opens are serialized a layer up (s.mu around
// lockedNS). The id is immutable once written: reopened namespaces read it
// (readNSGen) and never re-mint, so it is stable for the namespace's
// lifetime and fresh only after DropNamespace deleted the file.
func ensureNSGen(rw *sql.DB) error {
	// Zero is the seam's "no guard" sentinel (§6.2), so a mint that lands on
	// it (2^-128) is redrawn rather than stored: a real namespace must never
	// carry an id every guard would ignore.
	var zero, gen [16]byte
	for gen == zero {
		if _, err := rand.Read(gen[:]); err != nil {
			return fmt.Errorf("mint nsgen: %w", err)
		}
	}
	tx, err := rw.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO _dolmen_meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`,
		nsGenKey, gen[:]); err != nil {
		return err
	}
	var stored []byte
	if err := tx.QueryRow(
		`SELECT value FROM _dolmen_meta WHERE key = ?`, nsGenKey).Scan(&stored); err != nil {
		return err
	}
	if len(stored) != len(gen) {
		return fmt.Errorf("corrupt nsgen: %d bytes, want %d", len(stored), len(gen))
	}
	return tx.Commit()
}

// readNSGen returns the namespace's creation id from its registry.
// ensureNSGen runs on every open path before a namespace is handed out, so
// a missing row here is not a fresh namespace (those are ErrNotFound one
// layer up, in ns) but an out-of-band mutation of the registry — reported as
// the corruption it is, never papered over with a re-mint, which would hand
// a second lifetime the same file and silently break every nsGen guard.
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

// NamespaceState returns the namespace's creation id (§6.2): the read the
// API layer uses to build CreateTable's nsGen guard, ErrNotFound when the
// namespace is absent — the read never creates implicitly (ns opens, it
// does not create, §6.2's global rule). The id was minted at the
// namespace's first init and is stable for its lifetime: stable across
// reopen, distinct after drop+recreate (§3.4). It is never the zero value
// (see ensureNSGen), so the zero nsGen a caller passes as a guard keeps its
// one meaning — "no guard, auth off" — and can never collide with a real
// namespace's id.
// TODO(8c): auth is ignored while auth is off — slice 8c verifies the
// binding set atomically with the read.
func (s *Store) NamespaceState(ctx context.Context, nsName string, auth []AuthBinding) ([16]byte, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return [16]byte{}, err
	}
	return readNSGen(ctx, n.ro)
}
