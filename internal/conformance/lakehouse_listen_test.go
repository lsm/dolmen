package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseListenEngine interface {
	lakehouseChangesEngine
	DropTable(context.Context, string, string, store.Incarnation) error
	Listen(context.Context, string, string, store.Cursor, [16]byte, func(string) (*store.RowScope, store.Incarnation, bool), func(store.ChangeRecord), func(error)) (*store.ChangeReplay, func(), error)
}

func TestLakehouseListenBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			var raw namespaceEngine
			var err error
			if backend == "lakehouse" {
				raw, err = lakehouse.Open(t.TempDir())
			} else {
				raw, err = store.Open(t.TempDir())
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			eng, ok := raw.(lakehouseListenEngine)
			if !ok {
				t.Fatal("engine has no listen")
			}
			ctx := t.Context()
			ns := "project"
			none := store.Incarnation{}
			emb := store.Embedder{}
			if err := eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.CreateTable(ctx, ns, "t", []schema.Field{{Name: "x", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, ns, "t", []map[string]any{{"x": "before"}}, store.WriteOpts{}, emb, nil, none); err != nil {
				t.Fatal(err)
			}
			got := make(chan store.ChangeRecord, 16)
			ended := make(chan error, 1)
			replay, cancel, err := eng.Listen(ctx, ns, "t", store.CursorBegin, [16]byte{}, nil, func(r store.ChangeRecord) { got <- r }, func(err error) { ended <- err })
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			var replayed []store.ChangeRecord
			for {
				recs, _, done, err := replay.Next(ctx)
				if err != nil {
					t.Fatal(err)
				}
				replayed = append(replayed, recs...)
				if done {
					break
				}
			}
			if len(replayed) != 1 || replayed[0].RowID != 1 {
				t.Fatalf("replay from begin = %+v", replayed)
			}
			if _, err := eng.Insert(ctx, ns, "t", []map[string]any{{"x": "after"}}, store.WriteOpts{}, emb, nil, none); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-got:
				if r.RowID != 2 || r.Kind != store.ChangeInsert || r.Cursor == "" {
					t.Fatalf("live record = %+v", r)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("a commit after listen must be delivered live")
			}
			if err := eng.DropTable(ctx, ns, "t", none); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-ended:
				if !errors.Is(err, store.ErrListenLifetimeEnded) {
					t.Fatalf("dropping the table ends the session with its lifetime: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("dropping the table must end the session")
			}
		})
	}
}
