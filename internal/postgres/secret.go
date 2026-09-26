package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) HasSecretKey() bool { return s.secrets != nil }

func (s *Store) sealWrite(f schema.Field, cv any) (any, error) {
	return store.SealSecret(s.secrets, f, cv)
}

func (s *Store) revealSet(ctx context.Context, sc *schema.TableSchema) (map[string]bool, error) {
	return store.RevealSet(ctx, sc, s.secrets)
}

func (s *Store) presentSecret(reveal map[string]bool, f schema.Field, v any) (any, bool, error) {
	if f.Type != schema.Secret || v == nil || !reveal[f.Name] {
		return nil, false, nil
	}
	plain, err := store.OpenSecret(s.secrets, f.Name, v)
	if err != nil {
		return nil, true, err
	}
	return plain, true, nil
}

func (s *Store) recordHash(sc *schema.TableSchema, records []map[string]any) (string, error) {
	raw, err := json.Marshal(store.FingerprintSecrets(s.secrets, sc, records))
	if err != nil {
		return "", fmt.Errorf("%w: cannot encode records: %v", store.ErrInvalid, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func addedFields(changes []schema.Change) []schema.Field {
	var out []schema.Field
	for _, ch := range changes {
		if ch.Op == schema.OpAddField && ch.Field != nil {
			out = append(out, *ch.Field)
		}
	}
	return out
}

var maskLiteral = "'" + strings.ReplaceAll(secret.Mask, "'", "''") + "'::text"
