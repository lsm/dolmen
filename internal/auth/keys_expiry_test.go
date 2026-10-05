package auth

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func authenticateKey(t *testing.T, r *Registry, secret string) bool {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	_, ok := (&keySource{reg: r}).Authenticate(req)
	return ok
}

func TestAKeyStopsAuthenticatingWhenItExpires(t *testing.T) {
	r := seedKeyRegistry(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	k, secret, err := r.CreateKeyExpiring(ctx, "contractor", "temp", nil, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if k.ExpiresAt.IsZero() || !k.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("expires_at = %v, want %v", k.ExpiresAt, now.Add(time.Hour))
	}
	if !authenticateKey(t, r, secret) {
		t.Fatal("a key before its expiry must authenticate")
	}
	now = now.Add(time.Hour)
	if authenticateKey(t, r, secret) {
		t.Fatal("a key at its expiry must stop authenticating")
	}
	active, err := r.activeKeysLocked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("an expired key must not count as active, so it cannot keep a root administrator usable: %v", active)
	}
	keys, err := r.ListKeys(ctx)
	if err != nil || len(keys) != 1 || !keys[0].Expired(now) {
		t.Fatalf("list_keys must keep the expired key and report it expired: %v %v", keys, err)
	}
}

func TestAKeyWithoutExpiryNeverExpires(t *testing.T) {
	r := seedKeyRegistry(t)
	_, secret, err := r.CreateKey(context.Background(), "ci", "ci-bot", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.Now().Add(100 * 365 * 24 * time.Hour) }
	if !authenticateKey(t, r, secret) {
		t.Fatal("a key minted without expires_at must keep authenticating")
	}
}

func TestARegistryFromBeforeKeyExpiryOpensAndKeepsItsKeys(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", registryDSN(filepath.Join(dir, RegistryFile)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE api_keys (
		id         TEXT PRIMARY KEY,
		hash       TEXT NOT NULL UNIQUE,
		name       TEXT NOT NULL,
		principal  TEXT NOT NULL,
		groups     TEXT NOT NULL,
		revoked    INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	secret, id, err := MintKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id, hash, name, principal, groups, revoked, created_at) VALUES (?, ?, 'old', 'old-bot', '', 0, ?)`,
		id, hashKey(secret), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("a registry written before key expiry must open: %v", err)
	}
	defer r.Close()
	if !authenticateKey(t, r, secret) {
		t.Fatal("a key minted before key expiry existed must keep authenticating, with no expiry")
	}
	if _, _, err := r.CreateKeyExpiring(context.Background(), "new", "new-bot", nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("the upgraded registry must store an expiry: %v", err)
	}
}
