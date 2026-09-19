package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type changesEngine interface {
	insertEngine
	ChangesSince(context.Context, string, string, store.Cursor, [16]byte, *store.RowScope, store.Incarnation, store.Page) ([]store.ChangeRecord, store.Cursor, error)
}

func TestChangesBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var eng changesEngine
			if backend == "postgres" {
				eng = postgresNamespaceEngine(t).(changesEngine)
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
			for _, table := range []string{"notes", "other"} {
				if _, err := eng.CreateTable(ctx, "app", table, []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
					t.Fatal(err)
				}
			}
			gen, err := eng.NamespaceState(ctx, "app", nil)
			if err != nil {
				t.Fatal(err)
			}
			initial, head, err := eng.ChangesSince(ctx, "app", "", "", gen, nil, store.Incarnation{}, store.Page{})
			if err != nil || initial == nil || len(initial) != 0 || head == "" {
				t.Fatalf("head: %+v %q %v", initial, head, err)
			}
			for _, table := range []string{"notes", "other", "notes"} {
				if _, err := eng.Insert(ctx, "app", table, []map[string]any{{"body": "value"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
					t.Fatal(err)
				}
			}
			page, next, err := eng.ChangesSince(ctx, "app", "", head, gen, nil, store.Incarnation{}, store.Page{Limit: 2})
			if err != nil || len(page) != 2 || page[0].Table != "notes" || page[1].Table != "other" {
				t.Fatalf("page: %+v %v", page, err)
			}
			for _, rec := range page {
				if rec.Kind != store.ChangeInsert || rec.Cursor == "" || rec.Lifetime.NsGen != gen || rec.Lifetime.Table != rec.Table || rec.Lifetime.DropGen != 0 {
					t.Fatalf("record: %+v", rec)
				}
			}
			rest, end, err := eng.ChangesSince(ctx, "app", "", next, gen, nil, store.Incarnation{}, store.Page{})
			if err != nil || len(rest) != 1 || rest[0].RowID != 2 {
				t.Fatalf("resume: %+v %v", rest, err)
			}
			empty, same, err := eng.ChangesSince(ctx, "app", "", end, gen, nil, store.Incarnation{}, store.Page{})
			if err != nil || len(empty) != 0 || same != end {
				t.Fatalf("stable empty resume: %q %q %v", end, same, err)
			}
			again, _, err := eng.ChangesSince(ctx, "app", "", page[0].Cursor, gen, nil, store.Incarnation{}, store.Page{})
			if err != nil || len(again) != 2 {
				t.Fatalf("per-event resume: %+v %v", again, err)
			}
			all, _, err := eng.ChangesSince(ctx, "app", "", store.CursorBegin, gen, nil, store.Incarnation{}, store.Page{})
			if err != nil || len(all) != 3 {
				t.Fatalf("begin: %+v %v", all, err)
			}
			scoped, token, err := eng.ChangesSince(ctx, "app", "notes", store.CursorBegin, gen, nil, store.Incarnation{}, store.Page{})
			if err != nil || len(scoped) != 2 {
				t.Fatalf("table feed: %+v %v", scoped, err)
			}
			if _, _, err := eng.ChangesSince(ctx, "app", "other", token, gen, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrCursorCrossFeed) {
				t.Fatalf("cross-feed token: %v", err)
			}
			if _, _, err := eng.ChangesSince(ctx, "app", "", "forged-token", gen, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrCursorExpired) {
				t.Fatalf("forged token: %v", err)
			}
		})
	}
}
