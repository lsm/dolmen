package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const MaxPendingSignIns = 512

func (r *Registry) initPending() error {
	if _, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS auth_pending (
		state        TEXT PRIMARY KEY,
		verifier     TEXT NOT NULL,
		redirect_uri TEXT NOT NULL,
		expires_at   TEXT NOT NULL
	)`); err != nil {
		return err
	}
	_, err := r.db.Exec(`CREATE INDEX IF NOT EXISTS auth_pending_expiry ON auth_pending(expires_at)`)
	return err
}

func (r *Registry) putPending(ctx context.Context, state, verifier, redirectURI string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM auth_pending WHERE expires_at <= ?`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("prune sign-in state: %w", err)
	}
	var pending int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM auth_pending`).Scan(&pending); err != nil {
		return fmt.Errorf("count sign-in state: %w", err)
	}
	if pending >= MaxPendingSignIns {
		return fmt.Errorf("%w: too many sign-ins are already in flight on this server (%d), so this one was not started; they expire within %s, so try again shortly", ErrAuthFlow, pending, pendingTTL)
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO auth_pending (state, verifier, redirect_uri, expires_at) VALUES (?, ?, ?, ?)`,
		state, verifier, redirectURI,
		time.Now().UTC().Add(pendingTTL).Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("store sign-in state: %w", err)
	}
	return nil
}

func (r *Registry) takePending(ctx context.Context, state string) (verifier, redirectURI string, ok bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var expires string
	err = r.db.QueryRowContext(ctx,
		`SELECT verifier, redirect_uri, expires_at FROM auth_pending WHERE state = ?`, state).
		Scan(&verifier, &redirectURI, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("read sign-in state: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM auth_pending WHERE state = ?`, state); err != nil {
		return "", "", false, fmt.Errorf("consume sign-in state: %w", err)
	}
	at, parseErr := time.Parse(time.RFC3339Nano, expires)
	if parseErr != nil || time.Now().UTC().After(at) {
		return "", "", false, nil
	}
	return verifier, redirectURI, true, nil
}
