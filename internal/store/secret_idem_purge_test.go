package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
)

func seedStaleSecretRecord(t *testing.T, dir string, k *secret.Keyring, stale string) {
	t.Helper()
	st := openSecretStore(t, dir, k)
	ctx := context.Background()
	mustNS(t, st, "test")
	if _, err := st.CreateTable(ctx, "test", "creds", []schema.Field{{Name: "name", Type: schema.String}, {Name: "token", Type: schema.Secret}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Store.Insert(ctx, "test", "creds", []map[string]any{{"name": "a", "token": "hunter2"}}, WriteOpts{IdempotencyKey: "old"}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	n, err := st.ns("test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = n.rw.ExecContext(ctx, `UPDATE `+idempotencyTable+` SET payload_hash = ? WHERE table_name = 'creds'`, stale)
	if err == nil {
		_, err = n.rw.ExecContext(ctx, `DELETE FROM _dolmen_meta WHERE key = ?`, secretIdemPurgedKey)
	}
	n.unpin()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
}

func assertNoBytes(t *testing.T, dir, needle, when string) {
	t.Helper()
	for _, name := range []string{"test.db", "test.db-wal"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(needle)) {
			t.Fatalf("%s: %s still holds the purged hash bytes", when, name)
		}
	}
}

func copyNSFiles(t *testing.T, from, to string) {
	t.Helper()
	for _, name := range []string{"test.db", "test.db-wal", "test.db-shm"} {
		os.Remove(filepath.Join(to, name))
		b, err := os.ReadFile(filepath.Join(from, name))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(to, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPurgeLeavesNoStaleHashBytesOnDisk(t *testing.T) {
	dir := t.TempDir()
	k := testKeyring(t, 1)
	stale := strings.Repeat("feedface", 8)
	seedStaleSecretRecord(t, dir, k, stale)
	st := openSecretStore(t, dir, k)
	if got := idemRows(t, st, "creds"); got != 0 {
		t.Fatalf("purge did not run, %d remain", got)
	}
	assertNoBytes(t, dir, stale, "after the purge with the namespace open")
	st.Close()
	assertNoBytes(t, dir, stale, "after close")
}

func TestPurgeRerunsWhenPrePurgeFileIsRestored(t *testing.T) {
	dir := t.TempDir()
	k := testKeyring(t, 1)
	seedStaleSecretRecord(t, dir, k, strings.Repeat("0badc0de", 8))
	saved := t.TempDir()
	copyNSFiles(t, dir, saved)
	st := openSecretStore(t, dir, k)
	if got := idemRows(t, st, "creds"); got != 0 {
		t.Fatalf("first purge did not run, %d remain", got)
	}
	st.Close()
	copyNSFiles(t, saved, dir)
	st = openSecretStore(t, dir, k)
	if got := idemRows(t, st, "creds"); got != 0 {
		t.Fatalf("a restored pre-purge file carries no marker, so the purge must run again; %d remain", got)
	}
}

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
