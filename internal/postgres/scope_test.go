package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresEveryScopedEntryPointConflictsOnAStaleIncarnation(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, stale, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DropTable(ctx, "app", "notes", stale); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, stale.NsGen); err != nil {
		t.Fatal(err)
	}
	record := []map[string]any{{"body": "x"}}
	calls := map[string]func() error{
		"DescribeTable": func() error {
			_, _, err := s.DescribeTable(ctx, "app", "notes", nil, stale)
			return err
		},
		"GetRows": func() error {
			_, err := s.GetRows(ctx, "app", "notes", []int64{1}, nil, stale)
			return err
		},
		"Insert": func() error {
			_, err := s.Insert(ctx, "app", "notes", record, store.WriteOpts{}, store.Embedder{}, nil, stale)
			return err
		},
		"Update": func() error {
			_, err := s.Update(ctx, "app", "notes", "1=1", nil, map[string]any{"body": "y"}, store.Embedder{}, nil, stale)
			return err
		},
		"Upsert": func() error {
			_, err := s.Upsert(ctx, "app", "notes", "1=1", nil, map[string]any{"body": "y"}, store.WriteOpts{}, store.Embedder{}, nil, stale)
			return err
		},
		"Delete": func() error {
			_, err := s.Delete(ctx, "app", "notes", "1=1", nil, store.DeleteOpts{}, nil, stale)
			return err
		},
		"UpsertByKey": func() error {
			_, err := s.UpsertByKey(ctx, "app", "notes", []string{"body"}, record, store.WriteOpts{}, store.Embedder{}, nil, stale)
			return err
		},
		"ChangesSince": func() error {
			_, _, err := s.ChangesSince(ctx, "app", "notes", store.CursorBegin, [16]byte{}, nil, stale, store.Page{})
			return err
		},
		"PlanMigration": func() error {
			changes := []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.Text}}}
			_, err := s.PlanMigration(ctx, "app", "notes", changes, store.Embedder{}, store.Incarnation{}, nil, stale)
			return err
		},
		"SearchFulltext": func() error {
			_, err := s.SearchFulltext(ctx, "app", "notes", "x", "", nil, false, nil, stale, store.Page{})
			return err
		},
		"SearchVector": func() error {
			q := store.VectorQuery{Vec: []float32{1}, Column: "body"}
			_, err := s.SearchVector(ctx, "app", "notes", q, false, nil, stale, store.Page{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("an incarnation resolved against the dropped table must conflict so the caller re-resolves authorization; %s answered %v", name, err)
			}
		})
	}
}

func TestPostgresAScopedWriteConflictsWhenTheTableIsReplacedWhileItEmbeds(t *testing.T) {
	record := []map[string]any{{"body": "x"}}
	calls := map[string]func(context.Context, *Store, store.Embedder, store.Incarnation) error{
		"Insert": func(ctx context.Context, s *Store, emb store.Embedder, inc store.Incarnation) error {
			_, err := s.Insert(ctx, "app", "notes", record, store.WriteOpts{}, emb, nil, inc)
			return err
		},
		"Upsert": func(ctx context.Context, s *Store, emb store.Embedder, inc store.Incarnation) error {
			_, err := s.Upsert(ctx, "app", "notes", "1=1", nil, map[string]any{"body": "x"}, store.WriteOpts{}, emb, nil, inc)
			return err
		},
		"UpsertByKey": func(ctx context.Context, s *Store, emb store.Embedder, inc store.Incarnation) error {
			_, err := s.UpsertByKey(ctx, "app", "notes", []string{"body"}, record, store.WriteOpts{}, emb, nil, inc)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			s := openTest(t, cfg)
			ctx := t.Context()
			if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{{Name: "body", Type: schema.Text, Vectorize: true}}
			if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			_, live, err := s.TableState(ctx, "app", "notes", nil)
			if err != nil {
				t.Fatal(err)
			}
			replaced := false
			emb := store.Embedder{Identity: "test", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
				if !replaced {
					replaced = true
					if err := s.DropTable(ctx, "app", "notes", live); err != nil {
						return nil, err
					}
					if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, live.NsGen); err != nil {
						return nil, err
					}
				}
				vecs := make([][]float32, len(texts))
				for i := range vecs {
					vecs[i] = []float32{1, 0}
				}
				return vecs, nil
			}}
			err = call(ctx, s, emb, live)
			if !replaced {
				t.Fatalf("%s never reached the embedder, so nothing replaced the table between its read and its write", name)
			}
			if !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("%s saw the table replaced between its read and its write and answered %v; the scope contract requires conflict so the caller re-resolves authorization", name, err)
			}
		})
	}
}
