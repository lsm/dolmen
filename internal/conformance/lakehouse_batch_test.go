package conformance

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseBatchEngine interface {
	lakehouseChangesEngine
	Batch(context.Context, string, []store.BatchWrite, store.BatchOpts, store.Embedder, *store.RowScope, store.Incarnation) (store.BatchResult, error)
}

func TestLakehouseBatchBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			var raw namespaceEngine
			var err error
			if backend == "lakehouse" {
				raw, err = lakehouse.Open(t.TempDir(), lakehouse.WithSQLEngine(lakehouseSQLEngine(t)))
			} else {
				raw, err = store.Open(t.TempDir())
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			eng, ok := raw.(lakehouseBatchEngine)
			if !ok {
				t.Fatal("engine has no batch")
			}
			ctx := t.Context()
			ns := "project"
			none := store.Incarnation{}
			emb := store.Embedder{}
			if err := eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				if _, err := eng.CreateTable(ctx, ns, name, []schema.Field{{Name: "x", Type: schema.String, Required: true}}, store.TableOpts{}, [16]byte{}); err != nil {
					t.Fatal(err)
				}
			}
			_, head, err := eng.ChangesSince(ctx, ns, "", "", [16]byte{}, nil, none, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			writes := []store.BatchWrite{
				{Kind: store.BatchWriteInsert, Table: "a", Records: []map[string]any{{"x": "1"}, {"x": "2"}}},
				{Kind: store.BatchWriteUpdate, Table: "a", Filter: "x = ?", Args: []any{"2"}, Set: map[string]any{"x": "2b"}},
				{Kind: store.BatchWriteInsert, Table: "b", Records: []map[string]any{{"x": "3"}}},
				{Kind: store.BatchWriteDelete, Table: "a", Filter: "x = '1'"},
			}
			res, err := eng.Batch(ctx, ns, writes, store.BatchOpts{IdempotencyKey: "k"}, emb, nil, none)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Results) != 4 || !reflect.DeepEqual(res.Results[0].Ids, []int64{1, 2}) || res.Results[1].Updated != 1 || res.Results[3].Deleted != 1 || res.Changes.Count != 5 {
				t.Fatalf("batch result = %+v", res)
			}
			rows, err := eng.GetRows(ctx, ns, "a", []int64{1, 2}, nil, none)
			if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["x"] != "2b" {
				t.Fatalf("a later write sees an earlier one: %+v %v", rows, err)
			}
			recs, _, err := eng.ChangesSince(ctx, ns, "", head, [16]byte{}, nil, none, store.Page{})
			if err != nil || len(recs) != 5 {
				t.Fatalf("feed after a batch = %+v %v", recs, err)
			}
			for _, r := range recs {
				if r.Commit != recs[0].Commit {
					t.Fatalf("a batch is one commit: %+v", recs)
				}
			}
			replay, err := eng.Batch(ctx, ns, writes, store.BatchOpts{IdempotencyKey: "k"}, emb, nil, none)
			if err != nil || !replay.Replayed || !reflect.DeepEqual(replay.Results[0].Ids, []int64{1, 2}) {
				t.Fatalf("a retried key replays: %+v %v", replay, err)
			}
			if _, err := eng.Batch(ctx, ns, writes[:1], store.BatchOpts{IdempotencyKey: "k"}, emb, nil, none); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("a reused key with another body conflicts: %v", err)
			}

			_, head, err = eng.ChangesSince(ctx, ns, "", "", [16]byte{}, nil, none, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = eng.Batch(ctx, ns, []store.BatchWrite{
				{Kind: store.BatchWriteInsert, Table: "b", Records: []map[string]any{{"x": "4"}}},
				{Kind: store.BatchWriteUpdate, Table: "a", Filter: "1=1", Set: map[string]any{"x": "gone"}},
				{Kind: store.BatchWriteInsert, Table: "a", Records: []map[string]any{{"nope": "5"}}},
			}, store.BatchOpts{}, emb, nil, none)
			if !errors.Is(err, store.ErrInvalid) || !strings.HasPrefix(err.Error(), "writes[2]: ") {
				t.Fatalf("a failing write names its index: %v", err)
			}
			if got, _, err := eng.ChangesSince(ctx, ns, "", head, [16]byte{}, nil, none, store.Page{}); err != nil || len(got) != 0 {
				t.Fatalf("a failed batch leaves no change records: %+v %v", got, err)
			}
			if b, err := eng.GetRows(ctx, ns, "b", []int64{2}, nil, none); err != nil || len(b.Rows) != 0 {
				t.Fatalf("a failed batch leaves no rows: %+v %v", b, err)
			}
			if a, err := eng.GetRows(ctx, ns, "a", []int64{2}, nil, none); err != nil || a.Rows[0]["x"] != "2b" {
				t.Fatalf("a failed batch leaves earlier rows unchanged: %+v %v", a, err)
			}
			if _, n, err := eng.DescribeTable(ctx, ns, "b", nil, none); err != nil || n != 1 {
				t.Fatalf("a failed batch leaves the row count: %d %v", n, err)
			}
			ok2, err := eng.Insert(ctx, ns, "b", []map[string]any{{"x": "6"}}, store.WriteOpts{}, emb, nil, none)
			if err != nil || !reflect.DeepEqual(ok2.Ids, []int64{2}) {
				t.Fatalf("ids a failed batch allocated are not consumed: %+v %v", ok2, err)
			}
		})
	}
}
