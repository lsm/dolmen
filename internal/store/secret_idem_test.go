package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
)

func TestIdempotencyHashNeverCoversSecretPlaintext(t *testing.T) {
	st := openSecretStore(t, t.TempDir(), testKeyring(t, 1))
	ctx := context.Background()
	mustNS(t, st, "test")
	if _, err := st.CreateTable(ctx, "test", "creds", []schema.Field{{Name: "name", Type: schema.String}, {Name: "token", Type: schema.Secret}}); err != nil {
		t.Fatal(err)
	}
	records := []map[string]any{{"name": "a", "token": "hunter2"}}
	opts := WriteOpts{IdempotencyKey: "k"}
	first, err := st.Store.Insert(ctx, "test", "creds", records, opts, testEmbed, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}

	n, err := st.ns("test")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	err = n.rw.QueryRowContext(ctx, `SELECT payload_hash FROM `+idempotencyTable+` WHERE table_name = 'creds' AND key = 'k'`).Scan(&stored)
	n.unpin()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(records)
	plainSum := sha256.Sum256(raw)
	if stored == hex.EncodeToString(plainSum[:]) {
		t.Fatal("the stored idempotency hash is SHA-256 over the plaintext secret, so a short secret can be brute-forced from it")
	}

	replay, err := st.Store.Insert(ctx, "test", "creds", []map[string]any{{"name": "a", "token": "hunter2"}}, opts, testEmbed, nil, Incarnation{})
	if err != nil || !replay.Replayed || replay.Ids[0] != first.Ids[0] {
		t.Fatalf("a replay carrying the same secret must match: %+v %v", replay, err)
	}
	_, err = st.Store.Insert(ctx, "test", "creds", []map[string]any{{"name": "a", "token": "hunter3"}}, opts, testEmbed, nil, Incarnation{})
	if err == nil || !strings.Contains(err.Error(), "different insert") {
		t.Fatalf("a different secret under the same key must conflict: %v", err)
	}

	if _, err := st.Store.Insert(ctx, "test", "creds", []map[string]any{{"name": "b", "token": "5"}}, WriteOpts{IdempotencyKey: "typed"}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, err = st.Store.Insert(ctx, "test", "creds", []map[string]any{{"name": "b", "token": 5}}, WriteOpts{IdempotencyKey: "typed"}, testEmbed, nil, Incarnation{})
	if err == nil || !strings.Contains(err.Error(), "different insert") {
		t.Fatalf("a replay sending the secret as a number must conflict, not replay the string write: %v", err)
	}
}

func TestRevealFailuresAreServerErrors(t *testing.T) {
	dir := t.TempDir()
	st := openSecretStore(t, dir, testKeyring(t, 1))
	seedSecrets(t, st)
	st.Close()
	reveal := WithReveal(context.Background(), []string{"token"})

	other := openSecretStore(t, dir, testKeyring(t, 2))
	_, err := other.GetRows(reveal, "test", "creds", []int64{1}, nil, Incarnation{})
	if err == nil || errors.Is(err, ErrInvalid) || !errors.Is(err, secret.ErrWrongKey) {
		t.Fatalf("a wrong key is a server misconfiguration, not an invalid request: %v", err)
	}
	for _, want := range []string{"key id", secret.EnvKey} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("wrong-key error %q does not name %q", err, want)
		}
	}
	other.Close()

	none := openSecretStore(t, dir, nil)
	_, err = none.GetRows(reveal, "test", "creds", []int64{1}, nil, Incarnation{})
	if err == nil || errors.Is(err, ErrInvalid) || !errors.Is(err, secret.ErrNoKey) {
		t.Fatalf("a missing key is a server misconfiguration, not an invalid request: %v", err)
	}
}
