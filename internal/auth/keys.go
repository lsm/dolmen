package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	KeyBytes      = 32
	KeyBodyLen    = 43
	KeyIDBytes    = 16
	MaxKeyName    = 128
	KeySourceName = "api-keys"
)

var keyBodyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

var ErrLastRootKey = errors.New("the deployment would be left with no usable root administrator")

type Key struct {
	ID        string
	Name      string
	Principal string
	Groups    []string
	Revoked   bool
	CreatedAt time.Time
	ExpiresAt time.Time
}

func (k Key) Expired(now time.Time) bool {
	return !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt)
}

const keyColumns = `id, name, principal, groups, revoked, created_at, expires_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanKey(row rowScanner, extra ...any) (Key, error) {
	var k Key
	var groups, created string
	var expires sql.NullString
	var revoked int
	if err := row.Scan(append([]any{&k.ID, &k.Name, &k.Principal, &groups, &revoked, &created, &expires}, extra...)...); err != nil {
		return Key{}, err
	}
	k.Groups = decodeGroups(groups)
	k.Revoked = revoked != 0
	var err error
	if k.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Key{}, fmt.Errorf("stored created_at %q is not a timestamp", created)
	}
	if expires.Valid && expires.String != "" {
		if k.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires.String); err != nil {
			return Key{}, fmt.Errorf("stored expires_at %q is not a timestamp", expires.String)
		}
	}
	return k, nil
}

func MintKey() (secret, id string, err error) {
	var body [KeyBytes]byte
	if _, err := rand.Read(body[:]); err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}
	var idBytes [KeyIDBytes]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", "", fmt.Errorf("generate key id: %w", err)
	}
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(body[:]), hex.EncodeToString(idBytes[:]), nil
}

func hashKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func ValidateKeyShape(secret string) bool {
	body, ok := strings.CutPrefix(secret, KeyPrefix)
	return ok && keyBodyRe.MatchString(body)
}

func ValidateKeyName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required: give the key a name so an administrator can tell it apart in list_keys")
	}
	if len(name) > MaxKeyName {
		return fmt.Errorf("name is %d bytes (max %d)", len(name), MaxKeyName)
	}
	if strings.ContainsAny(name, "\x00\n\r") {
		return fmt.Errorf("name must not contain control characters")
	}
	return nil
}

func ValidateKeyIdentity(principal string, groups []string, maxGroups int) error {
	if principal == "" {
		return fmt.Errorf("principal is required: the key authenticates as this identity, and grants are made to it")
	}
	if principal == AdminPrincipal {
		return fmt.Errorf("principal must not be %q: the bootstrap identity exists only while its credential does, and a minted key would outlive it", AdminPrincipal)
	}
	if !principalRe.MatchString(principal) {
		return fmt.Errorf("principal is not a usable identity: use 1 to 256 printable ASCII characters with no space")
	}
	if maxGroups <= 0 {
		maxGroups = DefaultMaxGroups
	}
	if len(groups) > maxGroups {
		return fmt.Errorf("the key carries %d groups (max %d): a key bearing an identity no request could assert would be unusable", len(groups), maxGroups)
	}
	seen := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		if !groupRe.MatchString(g) {
			return fmt.Errorf("group %q is not a usable group name: use 1 to 128 printable ASCII characters with no space or comma", g)
		}
		if _, dup := seen[g]; dup {
			return fmt.Errorf("group %q is listed twice", g)
		}
		seen[g] = struct{}{}
	}
	return nil
}

func (r *Registry) initKeys() error {
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS api_keys (
		id         TEXT PRIMARY KEY,
		hash       TEXT NOT NULL UNIQUE,
		name       TEXT NOT NULL,
		principal  TEXT NOT NULL,
		groups     TEXT NOT NULL,
		revoked    INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		expires_at TEXT
	)`)
	if err != nil {
		return err
	}
	var has int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('api_keys') WHERE name = 'expires_at'`).Scan(&has); err != nil {
		return err
	}
	if has == 0 {
		_, err = r.db.Exec(`ALTER TABLE api_keys ADD COLUMN expires_at TEXT`)
	}
	return err
}

func encodeGroups(groups []string) string { return strings.Join(groups, ",") }

func decodeGroups(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func (r *Registry) CreateKey(ctx context.Context, name, principal string, groups []string) (Key, string, error) {
	return r.CreateKeyExpiring(ctx, name, principal, groups, time.Time{})
}

func (r *Registry) CreateKeyExpiring(ctx context.Context, name, principal string, groups []string, expiresAt time.Time) (Key, string, error) {
	secret, id, err := MintKey()
	if err != nil {
		return Key{}, "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	created := r.clock().UTC()
	var expires any
	if !expiresAt.IsZero() {
		expiresAt = expiresAt.UTC()
		expires = expiresAt.Format(time.RFC3339Nano)
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, hash, name, principal, groups, revoked, created_at, expires_at) VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		id, hashKey(secret), name, principal, encodeGroups(groups), created.Format(time.RFC3339Nano), expires); err != nil {
		return Key{}, "", fmt.Errorf("store key: %w", err)
	}
	return Key{ID: id, Name: name, Principal: principal, Groups: groups, CreatedAt: created, ExpiresAt: expiresAt}, secret, nil
}

func (r *Registry) ListKeys(ctx context.Context) ([]Key, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+keyColumns+` FROM api_keys ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list keys: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (r *Registry) RevokeKey(ctx context.Context, id string, keepRootAdmin bool, reach Reach) (Key, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	k, found, err := r.keyByIDLocked(ctx, id)
	if err != nil {
		return Key{}, err
	}
	if !found {
		return Key{}, nil
	}
	if k.Revoked {
		return k, nil
	}
	if keepRootAdmin {
		reachable, err := r.rootReachableWithoutLocked(ctx, id, reach)
		if err != nil {
			return Key{}, err
		}
		if !reachable {
			return Key{}, ErrLastRootKey
		}
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE api_keys SET revoked = 1 WHERE id = ?`, id); err != nil {
		return Key{}, fmt.Errorf("revoke key: %w", err)
	}
	k.Revoked = true
	return k, nil
}

func (r *Registry) keyByIDLocked(ctx context.Context, id string) (Key, bool, error) {
	k, err := scanKey(r.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, fmt.Errorf("read key: %w", err)
	}
	return k, true, nil
}

func (r *Registry) activeKeysLocked(ctx context.Context) ([]Key, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+keyColumns+` FROM api_keys WHERE revoked = 0`)
	if err != nil {
		return nil, fmt.Errorf("read active keys: %w", err)
	}
	defer rows.Close()
	now := r.clock()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("read active keys: %w", err)
		}
		if !k.Expired(now) {
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

func (r *Registry) rootReachableWithoutLocked(ctx context.Context, excludeKeyID string, reach Reach) (bool, error) {
	admins, err := r.rootAdminsLocked(ctx)
	if err != nil {
		return false, err
	}
	if len(admins) == 0 {
		return true, nil
	}
	for _, a := range admins {
		if a.Type == SubjectPrincipal && reach.PrincipalReachable(a.ID) {
			return true, nil
		}
	}
	keys, err := r.activeKeysLocked(ctx)
	if err != nil {
		return false, err
	}
	return rootReachableByKey(admins, keys, excludeKeyID), nil
}

type keySource struct {
	reg *Registry
}

func (s *keySource) Name() string { return KeySourceName }

func (s *keySource) Authenticate(r *http.Request) (Identity, bool) {
	token, ok := BearerToken(r)
	if !ok || !ValidateKeyShape(token) {
		return Identity{}, false
	}
	k, ok, err := s.reg.keyByHash(r.Context(), hashKey(token))
	if err != nil || !ok || k.Revoked || k.Expired(s.reg.clock()) {
		return Identity{}, false
	}
	return Identity{Principal: k.Principal, Groups: k.Groups, Source: s.Name()}, true
}

func (r *Registry) keyByHash(ctx context.Context, hash string) (Key, bool, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+keyColumns+`, hash FROM api_keys`)
	if err != nil {
		return Key{}, false, fmt.Errorf("read keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var stored string
		k, err := scanKey(rows, &stored)
		if err != nil {
			return Key{}, false, fmt.Errorf("read keys: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) != 1 {
			continue
		}
		return k, true, nil
	}
	return Key{}, false, rows.Err()
}

func (r *Registry) ActiveKeys(ctx context.Context) ([]Key, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeKeysLocked(ctx)
}
