package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type rowReadEngine interface {
	tableEngine
	GetRows(context.Context, string, string, []int64, *store.RowScope, store.Incarnation) (store.QueryResult, error)
}

func TestRowReadBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var eng rowReadEngine
			if backend == "postgres" {
				eng = postgresNamespaceEngine(t).(rowReadEngine)
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
			if _, err := eng.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			for _, ids := range [][]int64{nil, {}, {2, 1, 2, -1, 999}, make([]int64, store.MaxReadRowsIDs)} {
				result, err := eng.GetRows(ctx, "app", "notes", ids, nil, store.Incarnation{})
				if err != nil || result.Rows == nil || len(result.Rows) != 0 || result.Truncated {
					t.Fatalf("empty page: %+v %v", result, err)
				}
			}
			if _, err := eng.GetRows(ctx, "app", "notes", make([]int64, store.MaxReadRowsIDs+1), nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("ID limit: %v", err)
			}
			for _, target := range [][2]string{{"app", "missing"}, {"missing", "notes"}} {
				if _, err := eng.GetRows(ctx, target[0], target[1], nil, nil, store.Incarnation{}); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("missing resource: %v", err)
				}
			}
		})
	}
}
