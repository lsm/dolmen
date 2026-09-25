package postgres

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

const pgSecret = "sk-pg-PLAINTEXT-7"

func pgKeyring(t *testing.T, b byte) *secret.Keyring {
	t.Helper()
	k, err := secret.New(bytes.Repeat([]byte{b}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pgSecretFields() []schema.Field {
	return []schema.Field{{Name: "label", Type: schema.String, Fulltext: true}, {Name: "token", Type: schema.Secret}}
}

func seedPGSecrets(t *testing.T, s *Store) store.Incarnation {
	t.Helper()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "creds", pgSecretFields(), store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "creds", []map[string]any{{"label": "alpha", "token": pgSecret}, {"label": "empty"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, inc, err := s.TableState(ctx, "app", "creds", nil)
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

func rawTokens(t *testing.T, s *Store, table string) [][]byte {
	t.Helper()
	ctx := t.Context()
	var out [][]byte
	err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT "+ident(state.columns["token"])+" FROM "+ident(n.physical, state.physical)+" ORDER BY id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func noPGPlaintext(t *testing.T, what string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "PLAINTEXT") {
		t.Fatalf("%s carries plaintext: %s", what, b)
	}
}

func TestPostgresSecretFieldsSealMaskAndReveal(t *testing.T) {
	cfg := testConfig(t)
	cfg.Secrets = pgKeyring(t, 1)
	s := openTest(t, cfg)
	ctx := t.Context()
	inc := seedPGSecrets(t, s)

	raw := rawTokens(t, s, "creds")
	if len(raw) != 2 || raw[1] != nil || bytes.Contains(raw[0], []byte("PLAINTEXT")) {
		t.Fatalf("stored tokens: %q", raw)
	}

	masked, err := s.GetRows(ctx, "app", "creds", []int64{1, 2}, nil, inc)
	if err != nil || masked.Rows[0]["token"] != secret.Mask || masked.Rows[1]["token"] != nil {
		t.Fatalf("masked read: %+v %v", masked.Rows, err)
	}
	revealCtx := store.WithReveal(ctx, []string{"token"})
	plain, err := s.GetRows(revealCtx, "app", "creds", []int64{1}, nil, inc)
	if err != nil || plain.Rows[0]["token"] != pgSecret {
		t.Fatalf("revealed read: %+v %v", plain.Rows, err)
	}
	ft, err := s.SearchFulltext(ctx, "app", "creds", "alpha", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 10})
	if err != nil || ft.Rows[0]["token"] != secret.Mask {
		t.Fatalf("masked search: %+v %v", ft, err)
	}
	ft, err = s.SearchFulltext(revealCtx, "app", "creds", "alpha", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 10})
	if err != nil || ft.Rows[0]["token"] != pgSecret {
		t.Fatalf("revealed search: %+v %v", ft, err)
	}
	q, err := s.Query(ctx, "app", "SELECT * FROM creds ORDER BY id", nil, [16]byte{}, store.Page{Limit: 10})
	if err != nil || q.Rows[0]["token"] != secret.Mask {
		t.Fatalf("query: %+v %v", q, err)
	}
	noPGPlaintext(t, "query", q)
	aliased, err := s.Query(ctx, "app", "SELECT token AS t FROM creds", nil, [16]byte{}, store.Page{Limit: 10})
	noPGPlaintext(t, "aliased query", aliased)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.Query(ctx, "app", "SELECT id FROM creds WHERE token = ?", []any{pgSecret}, [16]byte{}, store.Page{Limit: 10})
	if err == nil && len(hits.Rows) != 0 {
		t.Fatalf("a plaintext comparison matched %d rows", len(hits.Rows))
	}

	if _, err := s.GetRows(store.WithReveal(ctx, []string{"label"}), "app", "creds", []int64{1}, nil, inc); err == nil || !strings.Contains(err.Error(), "reveal lists only secret fields") {
		t.Fatalf("reveal of a non-secret field: %v", err)
	}

	if _, err := s.Update(ctx, "app", "creds", "label = ?", []any{"empty"}, map[string]any{"token": "second-PLAINTEXT"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertByKey(ctx, "app", "creds", []string{"label"}, []map[string]any{{"label": "gamma", "token": "third-PLAINTEXT"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	for i, b := range rawTokens(t, s, "creds") {
		if bytes.Contains(b, []byte("PLAINTEXT")) || len(b) == 0 {
			t.Fatalf("row %d stores %q", i+1, b)
		}
	}
	all, err := s.GetRows(revealCtx, "app", "creds", []int64{1, 2, 3}, nil, inc)
	if err != nil || all.Rows[1]["token"] != "second-PLAINTEXT" || all.Rows[2]["token"] != "third-PLAINTEXT" {
		t.Fatalf("reveal after update and upsert: %+v %v", all.Rows, err)
	}
	if _, err := s.UpsertByKey(ctx, "app", "creds", []string{"token"}, []map[string]any{{"label": "x", "token": "y"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err == nil || !strings.Contains(err.Error(), "fresh nonce") {
		t.Fatalf("a secret natural key: %v", err)
	}

	if _, err := s.Migrate(ctx, "app", "creds", []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "backup", Type: schema.Secret}}}, store.Embedder{}, store.Incarnation{}); err != nil {
		t.Fatalf("add a secret field: %v", err)
	}
	for option, change := range map[string]schema.Change{
		"fulltext":  {Op: schema.OpSetFulltext, Name: "token", Value: ptr(true)},
		"vectorize": {Op: schema.OpSetVectorize, Name: "token", Value: ptr(true)},
		"enum":      {Op: schema.OpSetEnum, Name: "token", Enum: &[]string{"a"}},
		"default":   {Op: schema.OpAddField, Field: &schema.Field{Name: "other", Type: schema.Secret}, Default: "x"},
	} {
		if _, err := s.Migrate(ctx, "app", "creds", []schema.Change{change}, store.Embedder{}, store.Incarnation{}); err == nil || !strings.Contains(err.Error(), "not allowed on secret fields") {
			t.Fatalf("migrate %s on a secret: %v", option, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestPostgresSecretKeyFailures(t *testing.T) {
	cfg := testConfig(t)
	cfg.Secrets = pgKeyring(t, 1)
	s := openTest(t, cfg)
	ctx := t.Context()
	inc := seedPGSecrets(t, s)
	reveal := store.WithReveal(ctx, []string{"token"})

	s.secrets = nil
	if _, err := s.CreateTable(ctx, "app", "more", pgSecretFields(), store.TableOpts{}, [16]byte{}); err == nil || !strings.Contains(err.Error(), secret.EnvKey) {
		t.Fatalf("create without a key: %v", err)
	}
	if _, err := s.Insert(ctx, "app", "creds", []map[string]any{{"label": "b", "token": "x"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err == nil || !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), secret.EnvKey) {
		t.Fatalf("write without a key: %v", err)
	}
	if got, err := s.GetRows(ctx, "app", "creds", []int64{1}, nil, inc); err != nil || got.Rows[0]["token"] != secret.Mask {
		t.Fatalf("masked read needs no key: %+v %v", got, err)
	}
	if _, err := s.GetRows(reveal, "app", "creds", []int64{1}, nil, inc); !errors.Is(err, secret.ErrNoKey) || errors.Is(err, store.ErrInvalid) {
		t.Fatalf("reveal without a key: %v", err)
	}

	s.secrets = pgKeyring(t, 2)
	if _, err := s.GetRows(reveal, "app", "creds", []int64{1}, nil, inc); !errors.Is(err, secret.ErrWrongKey) || errors.Is(err, store.ErrInvalid) {
		t.Fatalf("reveal under the wrong key: %v", err)
	}

	s.secrets = pgKeyring(t, 1)
	err := s.write(ctx, "app", inc.NsGen, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "creds")
		if err != nil {
			return err
		}
		col := ident(state.columns["token"])
		_, err = tx.Exec(ctx, "UPDATE "+ident(n.physical, state.physical)+" SET "+col+" = overlay("+col+" placing '\\xff'::bytea from length("+col+")) WHERE id = 1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRows(reveal, "app", "creds", []int64{1}, nil, inc); !errors.Is(err, secret.ErrTampered) {
		t.Fatalf("reveal of a tampered value: %v", err)
	}
}

func TestPostgresIdempotencyHashNeverCoversSecretPlaintext(t *testing.T) {
	cfg := testConfig(t)
	cfg.Secrets = pgKeyring(t, 1)
	s := openTest(t, cfg)
	ctx := t.Context()
	seedPGSecrets(t, s)
	records := []map[string]any{{"label": "a", "token": "hunter2"}}
	opts := store.WriteOpts{IdempotencyKey: "k"}
	first, err := s.Insert(ctx, "app", "creds", records, opts, store.Embedder{}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.pool.QueryRow(ctx, "SELECT payload_hash FROM "+s.relation("idempotency_owned")+" WHERE key='k'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(records)
	plainSum := sha256.Sum256(raw)
	if stored == hex.EncodeToString(plainSum[:]) {
		t.Fatal("the stored idempotency hash is SHA-256 over the plaintext secret")
	}
	replay, err := s.Insert(ctx, "app", "creds", []map[string]any{{"label": "a", "token": "hunter2"}}, opts, store.Embedder{}, nil, store.Incarnation{})
	if err != nil || !replay.Replayed || replay.Ids[0] != first.Ids[0] {
		t.Fatalf("a replay carrying the same secret must match: %+v %v", replay, err)
	}
	if _, err := s.Insert(ctx, "app", "creds", []map[string]any{{"label": "a", "token": "hunter3"}}, opts, store.Embedder{}, nil, store.Incarnation{}); err == nil || !strings.Contains(err.Error(), "different insert") {
		t.Fatalf("a different secret under the same key must conflict: %v", err)
	}
	if _, err := s.Insert(ctx, "app", "creds", []map[string]any{{"label": "b", "token": "5"}}, store.WriteOpts{IdempotencyKey: "typed"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "creds", []map[string]any{{"label": "b", "token": 5}}, store.WriteOpts{IdempotencyKey: "typed"}, store.Embedder{}, nil, store.Incarnation{}); err == nil || !strings.Contains(err.Error(), "different insert") {
		t.Fatalf("a number secret must not replay the string write: %v", err)
	}
}
