package conformance

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type keyUpsertEngine interface {
	changesEngine
	UpsertByKey(context.Context, string, string, []string, []map[string]any, store.WriteOpts, store.Embedder, *store.RowScope, store.Incarnation) (store.InsertResult, error)
}

func TestKeyUpsertBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var eng keyUpsertEngine
			if backend == "postgres" {
				eng = postgresNamespaceEngine(t).(keyUpsertEngine)
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
			fields := []schema.Field{{Name: "external_key", Type: schema.Number}, {Name: "body", Required: true}, {Name: "active", Type: schema.Boolean, Default: true}}
			if _, err := eng.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			apply := func(records []map[string]any) (store.InsertResult, error) {
				return eng.UpsertByKey(ctx, "app", "notes", []string{" EXTERNAL_KEY "}, records, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
			}
			first, err := apply([]map[string]any{{"external_key": int64(9007199254740993), "body": "original"}, {"external_key": int64(9007199254740993), "active": false}})
			if err != nil || first.Inserted != 1 || first.Updated != 1 || len(first.Ids) != 2 || first.Ids[0] != first.Ids[1] || first.Changes.Count != 2 {
				t.Fatalf("ordered batch: %+v %v", first, err)
			}
			rows, err := eng.GetRows(ctx, "app", "notes", first.Ids, nil, store.Incarnation{})
			if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["body"] != "original" || rows.Rows[0]["active"] != false {
				t.Fatalf("patch: %+v %v", rows, err)
			}
			for _, records := range [][]map[string]any{{{"external_key": 2}}, {{"external_key": nil, "body": "bad"}}, {{"external_key": 3, "body": "valid"}, {"external_key": 4}}, {{"external_key": int64(9007199254740993), "body": nil}}} {
				if _, err := apply(records); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("invalid batch accepted: %v", err)
				}
			}
			_, count, err := eng.DescribeTable(ctx, "app", "notes", nil, store.Incarnation{})
			if err != nil || count != 1 {
				t.Fatalf("partial batch committed: %d %v", count, err)
			}
			var wg sync.WaitGroup
			failures := make(chan error, 8)
			results := make(chan store.InsertResult, 8)
			for range 8 {
				wg.Go(func() {
					r, err := apply([]map[string]any{{"external_key": 5, "body": "concurrent"}})
					if err != nil {
						failures <- err
					} else {
						results <- r
					}
				})
			}
			wg.Wait()
			close(failures)
			close(results)
			for err := range failures {
				t.Error(err)
			}
			inserted := int64(0)
			var id int64
			for r := range results {
				inserted += r.Inserted
				if id == 0 {
					id = r.Ids[0]
				} else if id != r.Ids[0] {
					t.Fatal("concurrent upsert IDs differ")
				}
			}
			if inserted != 1 {
				t.Fatalf("concurrent inserts: %d", inserted)
			}
			if _, err := eng.Insert(ctx, "app", "notes", []map[string]any{{"external_key": 5, "body": "duplicate"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			if _, err := apply([]map[string]any{{"external_key": 5, "body": "ambiguous"}}); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("ambiguous key accepted: %v", err)
			}
			changes, _, err := eng.ChangesSince(ctx, "app", "notes", store.CursorBegin, [16]byte{}, nil, store.Incarnation{}, store.Page{})
			if err != nil || len(changes) != 11 || changes[0].Kind != store.ChangeInsert || changes[1].Kind != store.ChangeUpdate {
				t.Fatalf("change order/count: %d %v", len(changes), err)
			}
		})
	}
}
