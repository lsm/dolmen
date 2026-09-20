package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (r *Registry) initPending() error {
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS auth_pending (
		state        TEXT PRIMARY KEY,
		verifier     TEXT NOT NULL,
		redirect_uri TEXT NOT NULL,
		expires_at   TEXT NOT NULL
	)`)
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
