package auth

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

func (r *Registry) initKeyring() error {
	if _, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS signing_keys (
		id         TEXT PRIMARY KEY,
		private    TEXT NOT NULL,
		public     TEXT NOT NULL,
		active     INTEGER NOT NULL DEFAULT 0,
		retired    INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := r.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS signing_keys_one_active
		ON signing_keys(active) WHERE active = 1`); err != nil {
		return err
	}
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS deployment (
		only_row   INTEGER PRIMARY KEY CHECK (only_row = 1),
		id         TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`)
	return err
}

func (r *Registry) DeploymentID(ctx context.Context, pinned string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var stored string
	err := r.db.QueryRowContext(ctx, `SELECT id FROM deployment WHERE only_row = 1`).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id := pinned
		if id == "" {
			minted, mintErr := NewDeploymentID()
			if mintErr != nil {
				return "", mintErr
			}
			id = minted
		}
		if _, err := r.db.ExecContext(ctx,
			`INSERT INTO deployment (only_row, id, created_at) VALUES (1, ?, ?) ON CONFLICT(only_row) DO NOTHING`,
			id, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return "", fmt.Errorf("store deployment id: %w", err)
		}
		if err := r.db.QueryRowContext(ctx, `SELECT id FROM deployment WHERE only_row = 1`).Scan(&stored); err != nil {
			return "", fmt.Errorf("read deployment id: %w", err)
		}
		if pinned != "" && pinned != stored {
			return "", deploymentMismatch(pinned, stored)
		}
		return stored, nil
	case err != nil:
		return "", fmt.Errorf("read deployment id: %w", err)
	}
	if pinned != "" && pinned != stored {
		return "", deploymentMismatch(pinned, stored)
	}
	return stored, nil
}

func deploymentMismatch(pinned, stored string) error {
	return fmt.Errorf("DOLMEN_AUTH_OIDC_DEPLOYMENT_ID is %q but this data directory was created as %q: tokens are bound to the deployment id, so changing it would invalidate every live token and accept none; unset the variable to keep %q, or point at a different data directory", pinned, stored, stored)
}

func (r *Registry) LoadKeyring(ctx context.Context, deployment string) (Keyring, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadKeyringLocked(ctx, deployment)
}

func (r *Registry) loadKeyringLocked(ctx context.Context, deployment string) (Keyring, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, private, public, active FROM signing_keys WHERE retired = 0 ORDER BY created_at`)
	if err != nil {
		return Keyring{}, fmt.Errorf("read signing keys: %w", err)
	}
	defer rows.Close()

	k := Keyring{Deployment: deployment}
	var found bool
	for rows.Next() {
		var id, priv, pub string
		var active int
		if err := rows.Scan(&id, &priv, &pub, &active); err != nil {
			return Keyring{}, fmt.Errorf("read signing keys: %w", err)
		}
		privBytes, err := hex.DecodeString(priv)
		if err != nil {
			return Keyring{}, fmt.Errorf("read signing key %s: stored private key is not hex", id)
		}
		pubBytes, err := hex.DecodeString(pub)
		if err != nil {
			return Keyring{}, fmt.Errorf("read signing key %s: stored public key is not hex", id)
		}
		sk := SigningKey{ID: id, Private: ed25519.PrivateKey(privBytes), Public: ed25519.PublicKey(pubBytes)}
		if active != 0 {
			k.Active = sk
			found = true
			continue
		}
		k.Verify = append(k.Verify, sk)
	}
	if err := rows.Err(); err != nil {
		return Keyring{}, fmt.Errorf("read signing keys: %w", err)
	}
	if found {
		return k, nil
	}

	sk, err := NewSigningKey()
	if err != nil {
		return Keyring{}, err
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO signing_keys (id, private, public, active, retired, created_at) VALUES (?, ?, ?, 1, 0, ?)
		 ON CONFLICT DO NOTHING`,
		sk.ID, hex.EncodeToString(sk.Private), hex.EncodeToString(sk.Public),
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return Keyring{}, fmt.Errorf("store signing key: %w", err)
	}
	return r.loadKeyringLocked(ctx, deployment)
}

func (r *Registry) RotateSigningKey(ctx context.Context, deployment string, retirePredecessors bool) (Keyring, error) {
	r.mu.Lock()
	sk, err := NewSigningKey()
	if err != nil {
		r.mu.Unlock()
		return Keyring{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		r.mu.Unlock()
		return Keyring{}, err
	}
	defer tx.Rollback()
	demote := `UPDATE signing_keys SET active = 0`
	if retirePredecessors {
		demote = `UPDATE signing_keys SET active = 0, retired = 1`
	}
	if _, err := tx.ExecContext(ctx, demote); err != nil {
		r.mu.Unlock()
		return Keyring{}, fmt.Errorf("retire signing key: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO signing_keys (id, private, public, active, retired, created_at) VALUES (?, ?, ?, 1, 0, ?)`,
		sk.ID, hex.EncodeToString(sk.Private), hex.EncodeToString(sk.Public),
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		r.mu.Unlock()
		return Keyring{}, fmt.Errorf("store signing key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		r.mu.Unlock()
		return Keyring{}, err
	}
	r.mu.Unlock()
	return r.LoadKeyring(ctx, deployment)
}
