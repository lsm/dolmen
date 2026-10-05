package lakehouse

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

func scannedRows(t *testing.T, s *Store, ns, name string) (int64, int) {
	t.Helper()
	var rows int64
	var snaps int
	err := s.withNamespace(t.Context(), ns, func(n *namespace) error {
		state, err := loadTable(t.Context(), n, ns, name)
		if err != nil {
			return err
		}
		snaps = len(state.native.Metadata().Snapshots())
		if state.native.Metadata().CurrentSnapshot() == nil {
			return nil
		}
		tbl, err := state.native.Scan().ToArrowTable(t.Context())
		if err != nil {
			return err
		}
		defer tbl.Release()
		rows = tbl.NumRows()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows, snaps
}

func TestAppendMaterializesIntoIceberg(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "title", Type: schema.String}, {Name: "score", Type: schema.Number}, {Name: "ok", Type: schema.Boolean}, {Name: "v", Type: schema.Vector, Dim: 2}}
	if _, err := s.CreateTable(ctx, "ns", "t", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"title": "a", "score": 1, "ok": true, "v": []any{1, 0}}, {"title": "b", "score": 2.5}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if rows, snaps := scannedRows(t, s, "ns", "t"); rows != 2 || snaps != 1 {
		t.Fatalf("one insert must become one snapshot holding its rows: %d rows in %d snapshots", rows, snaps)
	}
}

func TestACommitLoggedButNotMaterializedIsRecoveredOnce(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "title", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	materializeHook = func() error { return errors.New("simulated crash before the Iceberg commit") }
	res, err := s.Insert(ctx, "ns", "t", []map[string]any{{"title": "a"}, {"title": "b"}, {"title": "c"}}, store.WriteOpts{IdempotencyKey: "k"}, store.Embedder{}, nil, store.Incarnation{})
	materializeHook = nil
	if err != nil {
		t.Fatalf("a write the log committed must be acknowledged: %v", err)
	}
	if len(res.Ids) != 3 {
		t.Fatalf("ids %v", res.Ids)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, count, err := s.DescribeTable(ctx, "ns", "t", nil, store.Incarnation{})
	if err != nil || count != 3 {
		t.Fatalf("row_count after recovery = %d, %v; want 3", count, err)
	}
	if rows, snaps := scannedRows(t, s, "ns", "t"); rows != 3 || snaps != 1 {
		t.Fatalf("recovery must materialize the logged commit exactly once: %d rows in %d snapshots", rows, snaps)
	}
	if err := s.withNamespace(ctx, "ns", func(n *namespace) error {
		n.pending = 1
		_, err := n.db.ExecContext(ctx, `UPDATE _dolmen_lakehouse_commits SET materialized = 0`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rows, snaps := scannedRows(t, s, "ns", "t"); rows != 3 || snaps != 1 {
		t.Fatalf("replaying an already materialized commit must not duplicate it: %d rows in %d snapshots", rows, snaps)
	}
	replay, err := s.Insert(ctx, "ns", "t", []map[string]any{{"title": "a"}, {"title": "b"}, {"title": "c"}}, store.WriteOpts{IdempotencyKey: "k"}, store.Embedder{}, nil, store.Incarnation{})
	if err != nil || !replay.Replayed {
		t.Fatalf("the key committed with the logged rows must replay: %+v %v", replay, err)
	}
}

func TestAnIdempotentRetryIsAnsweredBeforeValidation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "title", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"title": "a"}}, store.WriteOpts{IdempotencyKey: "k"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Insert(ctx, "ns", "t", []map[string]any{{"nope": "a"}}, store.WriteOpts{IdempotencyKey: "k"}, store.Embedder{}, nil, store.Incarnation{})
	if !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("a recorded key with a different body is a conflict before the body is validated: %v", err)
	}
}

func TestTheFirstVectorizedAppendPinsTheEmbeddingSpace(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "docs", []schema.Field{{Name: "body", Type: schema.Text, Vectorize: true}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	embed := func(identity string, dim int) store.Embedder {
		return store.Embedder{Identity: identity, Embed: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range out {
				out[i] = make([]float32, dim)
				out[i][0] = 1
			}
			return out, nil
		}}
	}
	if _, err := s.Insert(ctx, "ns", "docs", []map[string]any{{"body": "hello"}, {"body": ""}}, store.WriteOpts{}, embed("fake-a", 3), nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	sc, _, err := s.TableState(ctx, "ns", "docs", nil)
	if err != nil || sc.EmbedSpace != "fake-a" || sc.EmbedDim != 3 {
		t.Fatalf("the first embedded append must pin the space: %+v %v", sc, err)
	}
	if _, err := s.Insert(ctx, "ns", "docs", []map[string]any{{"body": "again"}}, store.WriteOpts{}, embed("fake-b", 3), nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a different provider must be refused once the space is pinned: %v", err)
	}
	if rows, _ := scannedRows(t, s, "ns", "docs"); rows != 2 {
		t.Fatalf("%d rows materialized, want 2", rows)
	}
}

func TestSecretsAreKeptOutOfTheDataFiles(t *testing.T) {
	key, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s, err := Open(dir, WithSecretKeyring(key))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "creds", []schema.Field{{Name: "label", Type: schema.String}, {Name: "token", Type: schema.Secret}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "creds", []map[string]any{{"label": "a", "token": "PLAINTEXT-token"}, {"label": "b"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := s.withNamespace(ctx, "ns", func(n *namespace) error {
		return n.db.QueryRowContext(ctx, `SELECT value FROM _dolmen_lakehouse_secrets WHERE row_id = 1 AND field = 'token'`).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	if len(stored) < 16 || bytes.Contains(stored, []byte("PLAINTEXT")) {
		t.Fatalf("the catalog must hold the sealed secret: %q", stored)
	}
	err = filepath.WalkDir(filepath.Join(dir, "ns.lakehouse", "data"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, stored) || bytes.Contains(raw, []byte("PLAINTEXT")) {
			t.Fatalf("%s holds the secret; data files are readable by the SQL sidecar", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
