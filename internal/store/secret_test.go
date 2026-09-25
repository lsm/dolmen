package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
)

const secretPlain = "sk-live-PLAINTEXT-7f3a9c"

func testKeyring(t *testing.T, b byte) *secret.Keyring {
	t.Helper()
	k, err := secret.New(bytes.Repeat([]byte{b}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func openSecretStore(t *testing.T, dir string, k *secret.Keyring) legacyStore {
	t.Helper()
	st, err := Open(dir, WithSecretKey(k))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return legacy(st)
}

func secretFields() []schema.Field {
	return []schema.Field{
		{Name: "name", Type: schema.String, Fulltext: true},
		{Name: "token", Type: schema.Secret},
		{Name: "emb", Type: schema.Vector, Dim: 3},
	}
}

func seedSecrets(t *testing.T, st legacyStore) {
	t.Helper()
	ctx := context.Background()
	mustNS(t, st, "test")
	if _, err := st.CreateTable(ctx, "test", "creds", secretFields()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "creds", []map[string]any{
		{"name": "alpha key", "token": secretPlain, "emb": []any{1.0, 0.0, 0.0}},
		{"name": "beta key", "emb": []any{0.0, 1.0, 0.0}},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Update(ctx, "test", "creds", "name = ?", []any{"beta key"}, map[string]any{"token": secretPlain + "-updated"}, testEmbed); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "creds", []string{"name"}, []map[string]any{{"name": "gamma key", "token": secretPlain + "-upserted"}}, testEmbed); err != nil {
		t.Fatalf("upsert by key: %v", err)
	}
	if _, err := st.Upsert(ctx, "test", "creds", "name = ?", []any{"delta key"}, map[string]any{"name": "delta key", "token": secretPlain + "-filtered"}, testEmbed); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "creds", []map[string]any{{"name": "null key", "token": nil}}, testEmbed); err != nil {
		t.Fatalf("insert null: %v", err)
	}
}

func assertNoPlaintext(t *testing.T, where string, v any) {
	t.Helper()
	if s := fmt.Sprint(v); strings.Contains(s, "PLAINTEXT") {
		t.Fatalf("%s carries plaintext: %s", where, s)
	}
}

func assertMasked(t *testing.T, where string, rows []map[string]any) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatalf("%s returned no rows", where)
	}
	for _, r := range rows {
		if r["name"] == "null key" {
			if r["token"] != nil {
				t.Fatalf("%s: a null secret must read null, got %v", where, r["token"])
			}
			continue
		}
		if r["token"] != secret.Mask {
			t.Fatalf("%s: token = %#v, want the mask", where, r["token"])
		}
	}
	assertNoPlaintext(t, where, rows)
}

func TestSecretRefusedWithoutKey(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	_, err := st.CreateTable(ctx, "test", "creds", secretFields())
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "DOLMEN_SECRET_KEY") {
		t.Fatalf("create without key: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "plain", []schema.Field{{Name: "name", Type: schema.String}}); err != nil {
		t.Fatal(err)
	}
	_, err = st.Migrate(ctx, "test", "plain", []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "token", Type: schema.Secret}}}, testEmbed, 0)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "DOLMEN_SECRET_KEY") {
		t.Fatalf("add_field without key: %v", err)
	}
}

func TestSecretMaskedOnEveryReadPath(t *testing.T) {
	st := openSecretStore(t, t.TempDir(), testKeyring(t, 1))
	seedSecrets(t, st)
	ctx := context.Background()

	res, err := st.GetRows(ctx, "test", "creds", []int64{1, 2, 3, 4, 5}, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 5 {
		t.Fatalf("rows = %v", res.Rows)
	}
	assertMasked(t, "read_rows", res.Rows)

	rows, _, err := st.Query(ctx, "test", "SELECT * FROM creds", nil, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertMasked(t, "query *", rows)
	rows, _, err = st.Query(ctx, "test", "SELECT token AS t, CAST(token AS TEXT) AS c, hex(token) AS h FROM creds", nil, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertNoPlaintext(t, "query aliased", rows)

	rows, _, err = st.SearchFulltext(ctx, "test", "creds", "key", 0, 10, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertMasked(t, "search_fulltext", rows)

	vres, err := st.SearchVector(ctx, "test", "creds", "emb", []float32{1, 0, 0}, "", 0, 10, false, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertMasked(t, "search_vector", vres.Rows)

	rows, _, err = st.SearchFulltext(ctx, "test", "creds", "key", 0, 10, false, "token = ?", []any{secretPlain})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a filter sees only ciphertext, so comparing it to plaintext must match nothing: %v", rows)
	}

	changes, _, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("expected change records")
	}
	assertNoPlaintext(t, "changes_since", changes)
}

func TestSecretReveal(t *testing.T) {
	st := openSecretStore(t, t.TempDir(), testKeyring(t, 1))
	seedSecrets(t, st)
	ctx := WithReveal(context.Background(), []string{"token"})

	res, err := st.GetRows(ctx, "test", "creds", []int64{1, 2, 3, 4, 5}, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"alpha key": secretPlain, "beta key": secretPlain + "-updated", "gamma key": secretPlain + "-upserted", "delta key": secretPlain + "-filtered", "null key": nil}
	for _, r := range res.Rows {
		if r["token"] != want[r["name"].(string)] {
			t.Fatalf("revealed %v = %#v", r["name"], r["token"])
		}
	}
	rows, _, err := st.SearchFulltext(ctx, "test", "creds", "alpha", 0, 10, false, "", nil)
	if err != nil || len(rows) != 1 || rows[0]["token"] != secretPlain {
		t.Fatalf("fulltext reveal: %v %v", rows, err)
	}
	vres, err := st.SearchVector(ctx, "test", "creds", "emb", []float32{1, 0, 0}, "", 0, 1, false, "", nil, nil)
	if err != nil || vres.Rows[0]["token"] != secretPlain {
		t.Fatalf("vector reveal: %v %v", vres.Rows, err)
	}

	for field, want := range map[string]string{"name": "reveal lists only secret fields", "nope": "not a field of table"} {
		_, err := st.GetRows(WithReveal(context.Background(), []string{field}), "test", "creds", []int64{1}, nil, Incarnation{})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Fatalf("reveal %s: %v", field, err)
		}
	}
}

func TestSecretWrongOrMissingKey(t *testing.T) {
	dir := t.TempDir()
	st := openSecretStore(t, dir, testKeyring(t, 1))
	seedSecrets(t, st)
	st.Close()

	ctx := context.Background()
	reveal := WithReveal(ctx, []string{"token"})
	other := openSecretStore(t, dir, testKeyring(t, 2))
	res, err := other.GetRows(ctx, "test", "creds", []int64{1}, nil, Incarnation{})
	if err != nil || res.Rows[0]["token"] != secret.Mask {
		t.Fatalf("masked read must not need the right key: %v %v", res.Rows, err)
	}
	_, err = other.GetRows(reveal, "test", "creds", []int64{1}, nil, Incarnation{})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("wrong key reveal: %v", err)
	}
	other.Close()

	none := openSecretStore(t, dir, nil)
	if res, err = none.GetRows(ctx, "test", "creds", []int64{1}, nil, Incarnation{}); err != nil || res.Rows[0]["token"] != secret.Mask {
		t.Fatalf("masked read without key: %v %v", res.Rows, err)
	}
	if _, err = none.GetRows(reveal, "test", "creds", []int64{1}, nil, Incarnation{}); err == nil || !strings.Contains(err.Error(), "DOLMEN_SECRET_KEY") {
		t.Fatalf("reveal without key: %v", err)
	}
	_, err = none.Insert(ctx, "test", "creds", []map[string]any{{"name": "x", "token": "y"}}, testEmbed)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "DOLMEN_SECRET_KEY") {
		t.Fatalf("write without key: %v", err)
	}
	if _, err = none.Insert(ctx, "test", "creds", []map[string]any{{"name": "x"}}, testEmbed); err != nil {
		t.Fatalf("a row that carries no secret value needs no key: %v", err)
	}
}

func TestSecretMigrationGuards(t *testing.T) {
	st := openSecretStore(t, t.TempDir(), testKeyring(t, 1))
	seedSecrets(t, st)
	ctx := context.Background()
	tr := true
	cases := map[string]schema.Change{
		"full-text index would hold the plaintext":  {Op: schema.OpSetFulltext, Name: "token", Value: &tr},
		"embedding is computed from the plaintext":  {Op: schema.OpSetVectorize, Name: "token", Value: &tr},
		"allowed values would disclose":             {Op: schema.OpSetEnum, Name: "token", Enum: &[]string{"a"}},
		"schema stores a default in plaintext":      {Op: schema.OpAddField, Field: &schema.Field{Name: "pin", Type: schema.Secret}, Default: "1234"},
		"secret values are encrypted under a fresh": {},
	}
	for want, ch := range cases {
		var err error
		if ch.Op == "" {
			_, _, _, err = st.UpsertByKey(ctx, "test", "creds", []string{"token"}, []map[string]any{{"token": "x"}}, testEmbed)
		} else {
			_, err = st.Migrate(ctx, "test", "creds", []schema.Change{ch}, testEmbed, 0)
		}
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	if _, err := st.Migrate(ctx, "test", "creds", []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "pin", Type: schema.Secret}}}, testEmbed, 0); err != nil {
		t.Fatalf("add_field secret with a key: %v", err)
	}
}

func TestSecretPlaintextNeverOnDisk(t *testing.T) {
	dir := t.TempDir()
	st := openSecretStore(t, dir, testKeyring(t, 1))
	seedSecrets(t, st)
	scan := func(root string) {
		t.Helper()
		seen := 0
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			seen++
			if bytes.Contains(b, []byte("PLAINTEXT")) {
				t.Fatalf("%s holds plaintext", p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if seen == 0 {
			t.Fatalf("no files under %s", root)
		}
	}
	scan(dir)
	rows, _, err := st.Query(context.Background(), "test", "SELECT * FROM creds__fts", nil, 0, 100)
	if err == nil {
		assertNoPlaintext(t, "fts table", rows)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	scan(dir)
	out := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(context.Background(), dir, out, nil); err != nil {
		t.Fatal(err)
	}
	scan(out)
}
