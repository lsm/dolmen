package store

import (
	"context"
	"encoding/json"
	"fmt"

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
	if s.secrets != nil {
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
	if f.Type != schema.Secret || cv == nil {
		return cv, nil
	}
	plain, ok := cv.(string)
	if !ok {
		return nil, invalidf("field %q: expected a string", f.Name)
	}
	if s.secrets == nil {
		return nil, invalidf("field %q is a secret field and cannot be written: %s", f.Name, secret.ErrNoKey)
	}
	blob, err := s.secrets.Seal(plain)
	if err != nil {
		return nil, err
	}
	return blob, nil
}

func (s *Store) readProjection(ctx context.Context, sc *schema.TableSchema, includeHidden bool) (*projection, error) {
	p := projectionFromSchema(sc, includeHidden)
	names := RevealFrom(ctx)
	if len(names) == 0 {
		return p, nil
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
	if s.secrets == nil {
		return nil, fmt.Errorf("reveal cannot decrypt without a key: %w", secret.ErrNoKey)
	}
	p.reveal = reveal
	p.secrets = s.secrets
	return p, nil
}

func (s *Store) secretFingerprint(v any) string {
	if s.secrets == nil {
		return "secret-unkeyed"
	}
	stored, ok := storedString(v)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			b = []byte(fmt.Sprintf("%v", v))
		}
		return "secret-unstorable:" + s.secrets.Fingerprint(fmt.Sprintf("%T:%s", v, b))
	}
	tag := "string"
	if _, isNumber := v.(json.Number); isNumber {
		tag = "number"
	}
	return "secret-" + tag + ":" + s.secrets.Fingerprint(stored)
}
