package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func idemRows(t *testing.T, st legacyStore, table string) int {
	t.Helper()
	n, err := st.ns("test")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	var c int
	if err := n.rw.QueryRowContext(context.Background(), `SELECT count(*) FROM `+idempotencyTable+` WHERE table_name = ?`, table).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOpenPurgesPlaintextIdempotencyHashesOnce(t *testing.T) {
	dir := t.TempDir()
	k := testKeyring(t, 1)
	st := openSecretStore(t, dir, k)
	ctx := context.Background()
	mustNS(t, st, "test")
	if _, err := st.CreateTable(ctx, "test", "creds", []schema.Field{{Name: "name", Type: schema.String}, {Name: "token", Type: schema.Secret}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "test", "plain", []schema.Field{{Name: "name", Type: schema.String}}); err != nil {
		t.Fatal(err)
	}
	records := []map[string]any{{"name": "a", "token": "hunter2"}}
	if _, err := st.Store.Insert(ctx, "test", "creds", records, WriteOpts{IdempotencyKey: "old"}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Store.Insert(ctx, "test", "plain", []map[string]any{{"name": "a"}}, WriteOpts{IdempotencyKey: "keep"}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	n, err := st.ns("test")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(records)
	sum := sha256.Sum256(raw)
	_, err = n.rw.ExecContext(ctx, `UPDATE `+idempotencyTable+` SET payload_hash = ? WHERE table_name = 'creds'`, hex.EncodeToString(sum[:]))
	if err == nil {
		_, err = n.rw.ExecContext(ctx, `DELETE FROM _dolmen_meta WHERE key = ?`, secretIdemPurgedKey)
	}
	n.unpin()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	st = openSecretStore(t, dir, k)
	if got := idemRows(t, st, "creds"); got != 0 {
		t.Fatalf("a record written before the keyed fingerprint may hold a plaintext-derived hash and must be purged; %d remain", got)
	}
	if got := idemRows(t, st, "plain"); got != 1 {
		t.Fatalf("a table without secret fields keeps its idempotency records, got %d", got)
	}
	if _, err := st.Store.Insert(ctx, "test", "creds", records, WriteOpts{IdempotencyKey: "new"}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st = openSecretStore(t, dir, k)
	if got := idemRows(t, st, "creds"); got != 1 {
		t.Fatalf("the purge runs once; a record written after it must survive a reopen, got %d", got)
	}
}
