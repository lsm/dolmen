package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type queryEngine interface {
	insertEngine
	Query(context.Context, string, string, []any, [16]byte, store.Page) (store.QueryResult, error)
}

func TestQueryBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var eng queryEngine
			if backend == "postgres" {
				eng = postgresNamespaceEngine(t).(queryEngine)
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
			if _, err := eng.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}, {Name: "n", Type: schema.Number}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, "app", "notes", []map[string]any{{"body": "one", "n": 1}, {"body": "two", "n": 2}, {"body": "three", "n": 3}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			result, err := eng.Query(ctx, "app", "SELECT body,n FROM notes WHERE n >= ? ORDER BY n", []any{1}, [16]byte{}, store.Page{Offset: 1, Limit: 1})
			if err != nil || len(result.Rows) != 1 || result.Rows[0]["body"] != "two" || result.Rows[0]["n"] != int64(2) || !result.Truncated {
				t.Fatalf("query page: %+v %v", result, err)
			}
			result, err = eng.Query(ctx, "app", "WITH q AS (SELECT body FROM notes WHERE n = ?) SELECT body FROM q", []any{3}, [16]byte{}, store.Page{})
			if err != nil || len(result.Rows) != 1 || result.Rows[0]["body"] != "three" {
				t.Fatalf("CTE: %+v %v", result, err)
			}
			if _, err := eng.Query(ctx, "app", "DELETE FROM notes", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("mutation accepted: %v", err)
			}
			if _, err := eng.Query(ctx, "app", "SELECT 1 AS x,2 AS x", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("duplicate labels: %v", err)
			}
		})
	}
}
