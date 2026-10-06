package lakehouse

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestDeletesArePositionDeletesAndReplayOnce(t *testing.T) {
	cfg := sidecarConfig(t)
	dir := t.TempDir()
	s, err := Open(dir, WithSQLEngine(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"v": "a"}, {"v": "b"}, {"v": "c"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	materializeHook = func() error { return errors.New("simulated crash before the Iceberg commit") }
	res, err := s.Delete(ctx, "ns", "t", "v = ?", []any{"b"}, store.DeleteOpts{}, nil, store.Incarnation{})
	materializeHook = nil
	if err != nil || res.Deleted != 1 {
		t.Fatalf("delete %+v %v", res, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, WithSQLEngine(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if rows, _ := scannedRows(t, s, "ns", "t"); rows != 2 {
		t.Fatalf("after recovering the logged delete the table reads %d rows, want 2", rows)
	}
	var positionDeletes, rewritten int
	if err := s.withNamespace(ctx, "ns", func(n *namespace) error {
		state, err := loadTable(ctx, n, "ns", "t")
		if err != nil {
			return err
		}
		for _, snap := range state.native.Metadata().Snapshots() {
			if snap.Summary == nil {
				continue
			}
			if snap.Summary.Properties["added-position-delete-files"] != "" {
				positionDeletes++
			}
			if snap.Summary.Properties["deleted-data-files"] != "" {
				rewritten++
			}
		}
		n.pending = 1
		_, err = n.db.ExecContext(ctx, `UPDATE _dolmen_lakehouse_commits SET materialized = 0`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if positionDeletes != 1 || rewritten != 0 {
		t.Fatalf("a point delete must add one position-delete file and rewrite nothing: %d position-delete snapshots, %d rewrites", positionDeletes, rewritten)
	}
	if rows, _ := scannedRows(t, s, "ns", "t"); rows != 2 {
		t.Fatalf("replaying materialized commits must change nothing: %d rows", rows)
	}
}

func TestAnUpsertInsertPinsItsEmbeddingSpaceInItsCommit(t *testing.T) {
	cfg := sidecarConfig(t)
	s, err := Open(t.TempDir(), WithSQLEngine(cfg))
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
	emb := store.Embedder{Identity: "fake-a", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}}
	materializeHook = func() error { return errors.New("simulated crash before the Iceberg commit") }
	_, err = s.Upsert(ctx, "ns", "docs", "body = ?", []any{"hello"}, map[string]any{"body": "hello"}, store.WriteOpts{}, emb, nil, store.Incarnation{})
	materializeHook = nil
	_ = err
	var raw []byte
	if err := s.namespaces["ns"].db.QueryRowContext(ctx, `SELECT rows FROM _dolmen_lakehouse_commits WHERE materialized = 0 ORDER BY rowid DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var decoded commitRows
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.EmbedSpace != "fake-a" || decoded.EmbedDim != 3 {
		t.Fatalf("the upsert's commit must carry the embedding pin: %+v", decoded)
	}
}
