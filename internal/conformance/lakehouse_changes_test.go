package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseChangesEngine interface {
	lakehouseMutationEngine
	ChangesSince(context.Context, string, string, store.Cursor, [16]byte, *store.RowScope, store.Incarnation, store.Page) ([]store.ChangeRecord, store.Cursor, error)
}

func TestLakehouseChangeFeedBackendConformance(t *testing.T) {
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
			eng, ok := raw.(lakehouseChangesEngine)
			if !ok {
				t.Fatal("engine has no change feed")
			}
			ctx := t.Context()
			ns := "project"
			none := store.Incarnation{}
			emb := store.Embedder{}
			if err := eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				if _, err := eng.CreateTable(ctx, ns, name, []schema.Field{{Name: "x", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
					t.Fatal(err)
				}
			}
			_, head, err := eng.ChangesSince(ctx, ns, "", "", [16]byte{}, nil, none, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, ns, "a", []map[string]any{{"x": "1"}, {"x": "2"}}, store.WriteOpts{}, emb, nil, none); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, ns, "b", []map[string]any{{"x": "3"}}, store.WriteOpts{}, emb, nil, none); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Update(ctx, ns, "a", "id = 1", nil, map[string]any{"x": "1b"}, emb, nil, none); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Delete(ctx, ns, "a", "id = 2", nil, store.DeleteOpts{}, nil, none); err != nil {
				t.Fatal(err)
			}
			recs, next, err := eng.ChangesSince(ctx, ns, "", head, [16]byte{}, nil, none, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			want := []struct {
				table string
				id    int64
				kind  store.ChangeKind
			}{{"a", 1, store.ChangeInsert}, {"a", 2, store.ChangeInsert}, {"b", 1, store.ChangeInsert}, {"a", 1, store.ChangeUpdate}, {"a", 2, store.ChangeDelete}}
			if len(recs) != len(want) {
				t.Fatalf("namespace feed = %+v", recs)
			}
			for i, w := range want {
				if recs[i].Table != w.table || recs[i].RowID != w.id || recs[i].Kind != w.kind || recs[i].Cursor == "" {
					t.Fatalf("record %d = %+v, want %+v", i, recs[i], w)
				}
			}
			if recs[0].Commit != recs[1].Commit || recs[1].Commit == recs[2].Commit {
				t.Fatalf("one write is one commit: %+v", recs)
			}
			again, same, err := eng.ChangesSince(ctx, ns, "", next, [16]byte{}, nil, none, store.Page{})
			if err != nil || len(again) != 0 || same != next {
				t.Fatalf("a caught-up cursor returns nothing and itself: %v %q %v", again, same, err)
			}
			paged, cur, err := eng.ChangesSince(ctx, ns, "", head, [16]byte{}, nil, none, store.Page{Limit: 2})
			if err != nil || len(paged) != 2 {
				t.Fatalf("page = %v %v", paged, err)
			}
			rest, _, err := eng.ChangesSince(ctx, ns, "", cur, [16]byte{}, nil, none, store.Page{})
			if err != nil || len(rest) != 3 || rest[0].Table != "b" {
				t.Fatalf("resume after a page = %+v %v", rest, err)
			}
			fromRecord, _, err := eng.ChangesSince(ctx, ns, "", recs[3].Cursor, [16]byte{}, nil, none, store.Page{})
			if err != nil || len(fromRecord) != 1 || fromRecord[0].Kind != store.ChangeDelete {
				t.Fatalf("resume from a record cursor = %+v %v", fromRecord, err)
			}
			begin, tcur, err := eng.ChangesSince(ctx, ns, "a", store.CursorBegin, [16]byte{}, nil, none, store.Page{})
			if err != nil || len(begin) != 4 {
				t.Fatalf("table feed from begin = %+v %v", begin, err)
			}
			if _, _, err := eng.ChangesSince(ctx, ns, "b", tcur, [16]byte{}, nil, none, store.Page{}); !errors.Is(err, store.ErrCursorCrossFeed) {
				t.Fatalf("a cursor from another feed must be refused: %v", err)
			}
			if _, _, err := eng.ChangesSince(ctx, ns, "", "nonsense", [16]byte{}, nil, none, store.Page{}); err == nil {
				t.Fatal("an unknown cursor must be refused")
			}
			if _, _, err := eng.ChangesSince(ctx, ns, "", "", [16]byte{}, &store.RowScope{Owner: "x"}, none, store.Page{}); !errors.Is(err, store.ErrScopedNamespaceFeed) {
				t.Fatalf("a scoped namespace feed must be refused: %v", err)
			}

			if _, err := eng.CreateTable(ctx, ns, "mine", []schema.Field{{Name: "x", Type: schema.String}}, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			_, mhead, err := eng.ChangesSince(ctx, ns, "mine", "", [16]byte{}, nil, none, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			for _, owner := range []string{"alice", "bob", "alice"} {
				if _, err := eng.Insert(ctx, ns, "mine", []map[string]any{{"x": owner}}, store.WriteOpts{Owner: owner}, emb, nil, none); err != nil {
					t.Fatal(err)
				}
			}
			mine, _, err := eng.ChangesSince(ctx, ns, "mine", mhead, [16]byte{}, &store.RowScope{Owner: "alice"}, none, store.Page{})
			if err != nil || len(mine) != 2 || mine[0].Owner != "alice" || mine[1].RowID != 3 {
				t.Fatalf("a scoped feed shows only the owner's changes: %+v %v", mine, err)
			}
		})
	}
}
