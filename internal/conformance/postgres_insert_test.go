package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type insertEngine interface {
	rowReadEngine
	Insert(context.Context, string, string, []map[string]any, store.WriteOpts, store.Embedder, *store.RowScope, store.Incarnation) (store.InsertResult, error)
}

func TestInsertBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var eng insertEngine
			if backend == "postgres" {
				eng = postgresNamespaceEngine(t).(insertEngine)
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
			fields := []schema.Field{{Name: "body", Required: true}, {Name: "n", Type: schema.Number}, {Name: "active", Type: schema.Boolean, Default: true}, {Name: "metadata", Type: schema.JSON}, {Name: "vec", Type: schema.Vector, Dim: 2}, {Name: "stamp", Type: schema.Timestamp, Default: "now()"}}
			if _, err := eng.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			records := []map[string]any{{"BODY": "first", "n": int64(9007199254740993), "metadata": map[string]any{"tiny": json.Number("1e-400")}, "vec": []float64{0.5, -1}}, {"body": "second", "n": int64(-9223372036854775808), "active": nil}}
			opts := store.WriteOpts{IdempotencyKey: "retry"}
			first, err := eng.Insert(ctx, "app", "notes", records, opts, store.Embedder{}, nil, store.Incarnation{})
			if err != nil {
				t.Fatal(err)
			}
			if first.Replayed || len(first.Ids) != 2 || first.Changes.Count != 2 {
				t.Fatalf("insert: %+v", first)
			}
			replay, err := eng.Insert(ctx, "app", "notes", records, opts, store.Embedder{}, nil, store.Incarnation{})
			if err != nil || !replay.Replayed || !reflect.DeepEqual(first.Ids, replay.Ids) || replay.Changes.Count != 0 {
				t.Fatalf("replay: %+v %v", replay, err)
			}
			if _, err := eng.Insert(ctx, "app", "notes", []map[string]any{{"body": "different"}}, opts, store.Embedder{}, nil, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("conflicting retry: %v", err)
			}
			rows, err := eng.GetRows(ctx, "app", "notes", first.Ids, nil, store.Incarnation{})
			if err != nil {
				t.Fatal(err)
			}
			if rows.Rows[0]["n"] != int64(9007199254740993) || rows.Rows[1]["n"] != int64(-9223372036854775808) || rows.Rows[0]["active"] != true || rows.Rows[1]["active"] != nil {
				t.Fatalf("types/defaults: %#v", rows.Rows)
			}
			if rows.Rows[0]["metadata"].(map[string]any)["tiny"] != json.Number("1e-400") || !reflect.DeepEqual(rows.Rows[0]["vec"], []float64{0.5, -1}) || len(rows.Rows[0]["stamp"].(string)) != 24 {
				t.Fatalf("typed defaults: %#v", rows.Rows[0])
			}
			for _, bad := range [][]map[string]any{nil, {{"body": "ok"}, {"n": 1}}, {{"body": "ok", "BODY": "duplicate"}}, {{"body": "ok", "extra": 1}}, {{"body": "ok", "vec": []float64{1}}}} {
				if _, err := eng.Insert(ctx, "app", "notes", bad, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("invalid insert accepted: %v", err)
				}
			}
			_, count, err := eng.DescribeTable(ctx, "app", "notes", nil, store.Incarnation{})
			if err != nil || count != 2 {
				t.Fatalf("partial insert: %d %v", count, err)
			}
			var wg sync.WaitGroup
			results := make(chan store.InsertResult, 6)
			failures := make(chan error, 6)
			for range 6 {
				wg.Go(func() {
					r, err := eng.Insert(ctx, "app", "notes", []map[string]any{{"body": "concurrent"}}, store.WriteOpts{IdempotencyKey: "parallel"}, store.Embedder{}, nil, store.Incarnation{})
					if err != nil {
						failures <- err
					} else {
						results <- r
					}
				})
			}
			wg.Wait()
			close(results)
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			committed := 0
			var id int64
			for r := range results {
				if !r.Replayed {
					committed++
				}
				if id == 0 {
					id = r.Ids[0]
				} else if id != r.Ids[0] {
					t.Fatal("retry IDs differ")
				}
			}
			if committed != 1 {
				t.Fatalf("concurrent commits %d", committed)
			}
		})
	}
}
