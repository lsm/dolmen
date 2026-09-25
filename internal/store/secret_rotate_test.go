package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
)

func rotatedKeyring(t *testing.T, active byte, retired ...byte) *secret.Keyring {
	t.Helper()
	var old [][]byte
	for _, b := range retired {
		old = append(old, bytes.Repeat([]byte{b}, secret.KeySize))
	}
	k, err := secret.New(bytes.Repeat([]byte{active}, secret.KeySize), old...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestIdempotentInsertReplaysAcrossKeyRotation(t *testing.T) {
	dir := t.TempDir()
	st := openSecretStore(t, dir, testKeyring(t, 1))
	ctx := context.Background()
	mustNS(t, st, "test")
	if _, err := st.CreateTable(ctx, "test", "creds", []schema.Field{{Name: "name", Type: schema.String}, {Name: "token", Type: schema.Secret}}); err != nil {
		t.Fatal(err)
	}
	opts := WriteOpts{IdempotencyKey: "k"}
	first, err := st.Store.Insert(ctx, "test", "creds", []map[string]any{{"name": "a", "token": "hunter2"}}, opts, testEmbed, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	rotated := openSecretStore(t, dir, rotatedKeyring(t, 2, 1))
	if _, err := rotated.RotateSecrets(ctx, "test", RotateOpts{}); err != nil {
		t.Fatal(err)
	}
	replay, err := rotated.Store.Insert(ctx, "test", "creds", []map[string]any{{"name": "a", "token": "hunter2"}}, opts, testEmbed, nil, Incarnation{})
	if err != nil || !replay.Replayed || replay.Ids[0] != first.Ids[0] {
		t.Fatalf("a replay across a key rotation must replay, not conflict: %+v %v", replay, err)
	}
	_, err = rotated.Store.Insert(ctx, "test", "creds", []map[string]any{{"name": "a", "token": "hunter3"}}, opts, testEmbed, nil, Incarnation{})
	if err == nil || !strings.Contains(err.Error(), "different insert") {
		t.Fatalf("a different secret under the same key must still conflict: %v", err)
	}
}
