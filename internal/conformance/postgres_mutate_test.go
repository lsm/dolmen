package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type mutationEngine interface {
	changesEngine
	Update(context.Context, string, string, string, []any, map[string]any, store.Embedder, *store.RowScope, store.Incarnation) (store.UpdateResult, error)
	Upsert(context.Context, string, string, string, []any, map[string]any, store.WriteOpts, store.Embedder, *store.RowScope, store.Incarnation) (store.InsertResult, error)
	Delete(context.Context, string, string, string, []any, store.DeleteOpts, *store.RowScope, store.Incarnation) (store.DeleteResult, error)
}

func TestMutationBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var eng mutationEngine
			if backend == "postgres" {
				eng = postgresNamespaceEngine(t).(mutationEngine)
			} else {
				s, err := store.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				eng = s
			}
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{{Name: "body", Required: true, Vectorize: true}, {Name: "code", Required: true}, {Name: "score", Type: schema.Number}, {Name: "active", Type: schema.Boolean, Default: true}}
			if _, err := eng.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			emb := store.Embedder{Identity: "test", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
				calls++
				vectors := make([][]float32, len(texts))
				for i := range vectors {
					vectors[i] = []float32{1, float32(i)}
				}
				return vectors, nil
			}}
			inserted, err := eng.Insert(ctx, "app", "notes", []map[string]any{{"body": "one", "code": "a", "score": 1}, {"body": "two", "code": "b", "score": 2}, {"body": "three", "code": "c", "score": 3, "active": false}}, store.WriteOpts{}, emb, nil, store.Incarnation{})
			if err != nil {
				t.Fatal(err)
			}
			beforeMiss := calls
			miss, err := eng.Update(ctx, "app", "notes", "score > ?", []any{100}, map[string]any{"body": "missing"}, emb, nil, store.Incarnation{})
			if err != nil || miss.Updated != 0 || calls != beforeMiss {
				t.Fatalf("no-match update: %+v calls=%d want=%d err=%v", miss, calls, beforeMiss, err)
			}
			for _, set := range []map[string]any{{"missing": 1}, {"score": "invalid"}, {"body": nil}} {
				if _, err := eng.Update(ctx, "app", "notes", "score > ?", []any{100}, set, emb, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("invalid no-match update accepted: %v", err)
				}
			}
			beforeInvalidUpsert := calls
			if _, err := eng.Upsert(ctx, "app", "notes", "score = ?", []any{404}, map[string]any{"body": "would embed"}, store.WriteOpts{}, emb, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) || calls != beforeInvalidUpsert {
				t.Fatalf("invalid insert fallback embedded: calls=%d want=%d err=%v", calls, beforeInvalidUpsert, err)
			}
			updated, err := eng.Update(ctx, "app", "notes", "score >= ? AND active = ?", []any{2, true}, map[string]any{"body": "changed"}, emb, nil, store.Incarnation{})
			if err != nil || updated.Updated != 1 || updated.Changes.Count != 1 {
				t.Fatalf("update: %+v %v", updated, err)
			}
			matched, err := eng.Upsert(ctx, "app", "notes", "score = ?", []any{1}, map[string]any{"score": 10}, store.WriteOpts{}, emb, nil, store.Incarnation{})
			if err != nil || matched.Updated != 1 || matched.Inserted != 0 || len(matched.Ids) != 1 {
				t.Fatalf("matched upsert: %+v %v", matched, err)
			}
			created, err := eng.Upsert(ctx, "app", "notes", "score = ?", []any{99}, map[string]any{"body": "new", "code": "d", "score": 99}, store.WriteOpts{}, emb, nil, store.Incarnation{})
			if err != nil || created.Inserted != 1 || created.Updated != 0 || len(created.Ids) != 1 {
				t.Fatalf("insert upsert: %+v %v", created, err)
			}
			rows, err := eng.GetRows(ctx, "app", "notes", append(inserted.Ids, created.Ids...), nil, store.Incarnation{})
			if err != nil || len(rows.Rows) != 4 || rows.Rows[0]["score"] != int64(10) || rows.Rows[1]["body"] != "changed" || rows.Rows[3]["active"] != true {
				t.Fatalf("mutated rows: %+v %v", rows, err)
			}
			preview, err := eng.Delete(ctx, "app", "notes", "score >= ?", []any{2}, store.DeleteOpts{DryRun: true}, nil, store.Incarnation{})
			if err != nil || preview.Matched != 4 || preview.Deleted != 0 {
				t.Fatalf("delete preview: %+v %v", preview, err)
			}
			if _, err := eng.Delete(ctx, "app", "notes", "score >= ?", []any{2}, store.DeleteOpts{Limit: 2}, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("delete limit accepted: %v", err)
			}
			deleted, err := eng.Delete(ctx, "app", "notes", "score >= ?", []any{2}, store.DeleteOpts{Limit: 2, Confirm: true}, nil, store.Incarnation{})
			if err != nil || deleted.Matched != 4 || deleted.Deleted != 4 || deleted.Changes.Count != 4 {
				t.Fatalf("delete: %+v %v", deleted, err)
			}
			for _, filter := range []string{"", "1=1; DELETE FROM notes", "score ="} {
				if _, err := eng.Update(ctx, "app", "notes", filter, nil, map[string]any{"score": 1}, emb, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("invalid filter %q accepted: %v", filter, err)
				}
			}
			changes, _, err := eng.ChangesSince(ctx, "app", "notes", store.CursorBegin, [16]byte{}, nil, store.Incarnation{}, store.Page{})
			if err != nil || len(changes) != 10 {
				t.Fatalf("change count: %d %v", len(changes), err)
			}
		})
	}
}
