package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseAppendEngine interface {
	lakehouseTableEngine
	Insert(context.Context, string, string, []map[string]any, store.WriteOpts, store.Embedder, *store.RowScope, store.Incarnation) (store.InsertResult, error)
}

func TestLakehouseAppendBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			var current namespaceEngine
			open := func() lakehouseAppendEngine {
				var raw namespaceEngine
				var err error
				if backend == "lakehouse" {
					raw, err = lakehouse.Open(dir)
				} else {
					raw, err = store.Open(dir)
				}
				if err != nil {
					t.Fatal(err)
				}
				current = raw
				eng, ok := raw.(lakehouseAppendEngine)
				if !ok {
					raw.Close()
					t.Fatal("engine has no append path")
				}
				return eng
			}
			eng := open()
			t.Cleanup(func() { current.Close() })
			ctx := t.Context()
			ns := "project"
			if err := eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{
				{Name: "title", Type: schema.String, Required: true},
				{Name: "score", Type: schema.Number},
				{Name: "done", Type: schema.Boolean, Default: false},
				{Name: "meta", Type: schema.JSON},
			}
			if _, err := eng.CreateTable(ctx, ns, "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			count := func(eng lakehouseAppendEngine, scope *store.RowScope, name string) int64 {
				t.Helper()
				_, n, err := eng.DescribeTable(ctx, ns, name, scope, store.Incarnation{})
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
			insert := func(eng lakehouseAppendEngine, name string, opts store.WriteOpts, records ...map[string]any) (store.InsertResult, error) {
				return eng.Insert(ctx, ns, name, records, opts, store.Embedder{}, nil, store.Incarnation{})
			}

			first, err := insert(eng, "notes", store.WriteOpts{}, map[string]any{"title": "a", "score": 1.5}, map[string]any{"title": "b", "meta": map[string]any{"k": []any{1, 2}}}, map[string]any{"title": "c", "done": true})
			if err != nil {
				t.Fatal(err)
			}
			if want := []int64{1, 2, 3}; !equalIDs(first.Ids, want) || first.Replayed {
				t.Fatalf("first insert = %+v, want ids %v", first, want)
			}
			if first.Changes.Count != 3 || first.Changes.Last-first.Changes.First != 2 {
				t.Fatalf("an insert of three rows must mint three consecutive change records: %+v", first.Changes)
			}
			if n := count(eng, nil, "notes"); n != 3 {
				t.Fatalf("row_count %d after three inserted rows", n)
			}

			_, err = insert(eng, "notes", store.WriteOpts{}, map[string]any{"title": "x", "nope": 1})
			if !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("an unknown field must be refused as invalid: %v", err)
			}
			_, err = insert(eng, "notes", store.WriteOpts{}, map[string]any{"score": 2})
			if !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("a missing required field must be refused as invalid: %v", err)
			}
			second, err := insert(eng, "notes", store.WriteOpts{}, map[string]any{"title": "d"})
			if err != nil {
				t.Fatal(err)
			}
			if !equalIDs(second.Ids, []int64{4}) {
				t.Fatalf("a refused insert consumed ids: next id %v, want 4", second.Ids)
			}

			key := store.WriteOpts{IdempotencyKey: "retry-1"}
			original, err := insert(eng, "notes", key, map[string]any{"title": "e"}, map[string]any{"title": "f"})
			if err != nil {
				t.Fatal(err)
			}
			replay, err := insert(eng, "notes", key, map[string]any{"title": "e"}, map[string]any{"title": "f"})
			if err != nil {
				t.Fatal(err)
			}
			if !replay.Replayed || !equalIDs(replay.Ids, original.Ids) || replay.Changes.Count != 0 {
				t.Fatalf("a retry under the same key must replay the original ids and mint nothing: %+v vs %+v", replay, original)
			}
			_, err = insert(eng, "notes", key, map[string]any{"title": "other"})
			if ops.Classify(err) != derr.Conflict {
				t.Fatalf("the same key with a different body must be a conflict, got %v", err)
			}
			if n := count(eng, nil, "notes"); n != 6 {
				t.Fatalf("row_count %d, want 6: replays and refusals must not change it", n)
			}

			current.Close()
			eng = open()
			if n := count(eng, nil, "notes"); n != 6 {
				t.Fatalf("row_count %d after reopen, want 6", n)
			}
			replay, err = insert(eng, "notes", key, map[string]any{"title": "e"}, map[string]any{"title": "f"})
			if err != nil || !replay.Replayed || !equalIDs(replay.Ids, original.Ids) {
				t.Fatalf("an idempotency key must survive a reopen: %+v %v", replay, err)
			}
			next, err := insert(eng, "notes", store.WriteOpts{}, map[string]any{"title": "g"})
			if err != nil || !equalIDs(next.Ids, []int64{7}) {
				t.Fatalf("ids must continue after a reopen and never be reused: %+v %v", next, err)
			}

			if err := eng.DropTable(ctx, ns, "notes", store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.CreateTable(ctx, ns, "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if n := count(eng, nil, "notes"); n != 0 {
				t.Fatalf("a recreated table starts empty, row_count %d", n)
			}
			fresh, err := insert(eng, "notes", key, map[string]any{"title": "e"}, map[string]any{"title": "f"})
			if err != nil || fresh.Replayed {
				t.Fatalf("an idempotency key must not replay across a drop and recreate: %+v %v", fresh, err)
			}

			owned := []schema.Field{{Name: "body", Type: schema.Text}}
			if _, err := eng.CreateTable(ctx, ns, "mine", owned, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			for _, owner := range []string{"alice", "alice", "bob"} {
				if _, err := insert(eng, "mine", store.WriteOpts{Owner: owner}, map[string]any{"body": owner}); err != nil {
					t.Fatal(err)
				}
			}
			if n := count(eng, nil, "mine"); n != 3 {
				t.Fatalf("table-wide row_count %d, want 3", n)
			}
			if n := count(eng, &store.RowScope{Owner: "alice"}, "mine"); n != 2 {
				t.Fatalf("alice's row_count %d, want 2", n)
			}
			if n := count(eng, &store.RowScope{Empty: true}, "mine"); n != 0 {
				t.Fatalf("an empty scope's row_count %d, want 0", n)
			}
		})
	}
}

func equalIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
