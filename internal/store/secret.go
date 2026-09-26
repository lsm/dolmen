package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
)

type revealKey struct{}

func WithReveal(ctx context.Context, fields []string) context.Context {
	if len(fields) == 0 {
		return ctx
	}
	return context.WithValue(ctx, revealKey{}, append([]string(nil), fields...))
}

func RevealFrom(ctx context.Context) []string {
	v, _ := ctx.Value(revealKey{}).([]string)
	return v
}

func (s *Store) HasSecretKey() bool { return s.secrets != nil }

func (s *Store) requireSecretKey(fields []schema.Field) error {
	return RequireSecretKey(s.secrets, fields)
}

func RequireSecretKey(k *secret.Keyring, fields []schema.Field) error {
	if k != nil {
		return nil
	}
	for _, f := range fields {
		if f.Type == schema.Secret {
			return invalidf("field %q has type secret, but %s", f.Name, secret.ErrNoKey)
		}
	}
	return nil
}

func (s *Store) coerceWrite(f schema.Field, v any) (any, error) {
	cv, err := coerceValue(f, v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return SealSecret(s.secrets, f, cv)
}

func SealSecret(k *secret.Keyring, f schema.Field, cv any) (any, error) {
	if f.Type != schema.Secret || cv == nil {
		return cv, nil
	}
	plain, ok := cv.(string)
	if !ok {
		return nil, invalidf("field %q: expected a string", f.Name)
	}
	if k == nil {
		return nil, invalidf("field %q is a secret field and cannot be written: %s", f.Name, secret.ErrNoKey)
	}
	blob, err := k.Seal(plain)
	if err != nil {
		return nil, err
	}
	return blob, nil
}

func (s *Store) readProjection(ctx context.Context, sc *schema.TableSchema, includeHidden bool) (*projection, error) {
	p := projectionFromSchema(sc, includeHidden)
	reveal, err := RevealSet(ctx, sc, s.secrets)
	if err != nil || reveal == nil {
		return p, err
	}
	p.reveal = reveal
	p.secrets = s.secrets
	return p, nil
}

func RevealSet(ctx context.Context, sc *schema.TableSchema, k *secret.Keyring) (map[string]bool, error) {
	names := RevealFrom(ctx)
	if len(names) == 0 {
		return nil, nil
	}
	reveal := make(map[string]bool, len(names))
	for _, name := range names {
		f := sc.Field(name)
		if f == nil {
			return nil, invalidf("reveal names %q, which is not a field of table %s (see describe_table)", name, sc.Name)
		}
		if f.Type != schema.Secret {
			return nil, invalidf("reveal names %q, a %s field; reveal lists only secret fields, and every other field is already returned in full", name, f.Type)
		}
		reveal[name] = true
	}
	if k == nil {
		return nil, fmt.Errorf("reveal cannot decrypt without a key: %w", secret.ErrNoKey)
	}
	return reveal, nil
}

func OpenSecret(k *secret.Keyring, col string, v any) (string, error) {
	raw, isBlob := v.([]byte)
	if !isBlob {
		return "", fmt.Errorf("field %q: %w", col, secret.ErrCorrupt)
	}
	plain, err := k.Open(raw)
	if err != nil {
		return "", fmt.Errorf("field %q: %w", col, err)
	}
	return plain, nil
}

func FingerprintSecrets(k *secret.Keyring, sc *schema.TableSchema, records []map[string]any) []map[string]any {
	hashed := make([]map[string]any, len(records))
	for i, rec := range records {
		hashed[i] = make(map[string]any, len(rec))
		for name, v := range rec {
			if f := sc.Field(name); f != nil && f.Type == schema.Secret && v != nil {
				v = SecretFingerprint(k, v)
			}
			hashed[i][name] = v
		}
	}
	return hashed
}

func SecretFingerprint(k *secret.Keyring, v any) string {
	if k == nil {
		return "secret-unkeyed"
	}
	stored, ok := storedString(v)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			b = []byte(fmt.Sprintf("%v", v))
		}
		return "secret-unstorable:" + k.Fingerprint(fmt.Sprintf("%T:%s", v, b))
	}
	tag := "string"
	if _, isNumber := v.(json.Number); isNumber {
		tag = "number"
	}
	return "secret-" + tag + ":" + k.Fingerprint(stored)
}

func maskedProjections(ctx context.Context, tx queryRunner, nsName string, rewrites []tableRewrite) (map[string]string, error) {
	var out map[string]string
	seen := map[string]bool{}
	for _, r := range rewrites {
		if seen[r.table] {
			continue
		}
		seen[r.table] = true
		sc, err := loadSchema(ctx, tx, nsName, r.table)
		if err != nil {
			return nil, err
		}
		if !hasSecretField(sc) {
			continue
		}
		cols, err := queryColumns(ctx, tx, r.table)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]string{}
		}
		out[r.table] = maskedProjection(sc, cols)
	}
	return out, nil
}

func queryColumns(ctx context.Context, tx queryRunner, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

type queryRunner interface {
	rowQuerier
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

var maskLiteral = "'" + strings.ReplaceAll(secret.Mask, "'", "''") + "'"

func hasSecretField(sc *schema.TableSchema) bool {
	for _, f := range sc.Fields {
		if f.Type == schema.Secret {
			return true
		}
	}
	return false
}

func maskedProjection(sc *schema.TableSchema, cols map[string]bool) string {
	secrets := map[string]bool{}
	for _, f := range sc.Fields {
		if f.Type == schema.Secret {
			secrets[f.Name] = true
		}
	}
	names := make([]string, 0, len(cols))
	for name := range cols {
		names = append(names, name)
	}
	sort.Strings(names)
	ordered := []string{"id", "created_at"}
	for _, f := range sc.Fields {
		ordered = append(ordered, f.Name)
	}
	ordered = append(ordered, schema.OwnerColumn, "_embedding")
	listed := map[string]bool{}
	projected := make([]string, 0, len(names))
	add := func(name string) {
		if !cols[name] || listed[name] {
			return
		}
		listed[name] = true
		if secrets[name] {
			projected = append(projected, "CASE WHEN "+q(name)+" IS NULL THEN NULL ELSE "+maskLiteral+" END AS "+q(name))
			return
		}
		projected = append(projected, q(name))
	}
	for _, name := range ordered {
		add(name)
	}
	for _, name := range names {
		add(name)
	}
	return "(SELECT " + strings.Join(projected, ", ") + " FROM " + q(sc.Name) + ")"
}
