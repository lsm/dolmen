package conformance

import (
	"context"
	"errors"
	"strings"
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
			result, err = eng.Query(ctx, "app", "SELECT body FROM notes WHERE n BETWEEN ? AND ? ORDER BY n", []any{2, 3}, [16]byte{}, store.Page{})
			if err != nil || len(result.Rows) != 2 || result.Rows[0]["body"] != "two" || result.Rows[1]["body"] != "three" {
				t.Fatalf("BETWEEN: %+v %v", result, err)
			}
			if _, err := eng.Query(ctx, "app", "DELETE FROM notes", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("mutation accepted: %v", err)
			}
			if _, err := eng.Query(ctx, "app", "SELECT 1 AS x,2 AS x", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("duplicate labels: %v", err)
			}
			result, err = eng.Query(ctx, "app", "SELECT 1,2", nil, [16]byte{}, store.Page{})
			if err != nil || len(result.Rows) != 1 || len(result.Rows[0]) != 2 {
				t.Fatalf("unnamed columns: %+v %v", result, err)
			}
			values := map[int64]bool{}
			for _, value := range result.Rows[0] {
				if number, ok := value.(int64); ok {
					values[number] = true
				}
			}
			if !values[1] || !values[2] {
				t.Fatalf("unnamed column values: %+v", result.Rows[0])
			}
			args := make([]any, 101)
			if _, err := eng.Query(ctx, "app", "SELECT "+strings.TrimSuffix(strings.Repeat("?,", len(args)), ","), args, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("parameter cap: %v", err)
			}
			if _, err := eng.Query(ctx, "app", "SELECT '"+strings.Repeat("é", store.MaxQueryRunes)+"'", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("query length cap: %v", err)
			}
			if _, err := eng.CreateTable(ctx, "app", "vectors_two", []schema.Field{{Name: "v", Type: schema.Vector, Dim: 2}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.CreateTable(ctx, "app", "vectors_three", []schema.Field{{Name: "v", Type: schema.Vector, Dim: 3}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, "app", "vectors_two", []map[string]any{{"v": []float64{1, 2}}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, "app", "vectors_three", []map[string]any{{"v": []float64{1, 2, 3}}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			result, err = eng.Query(ctx, "app", "SELECT v FROM vectors_two UNION ALL SELECT v FROM vectors_three", nil, [16]byte{}, store.Page{})
			if err != nil || len(result.Rows) != 2 {
				t.Fatalf("mixed vector dimensions: %+v %v", result, err)
			}
			for _, row := range result.Rows {
				if _, ok := row["v"].([]float64); !ok {
					t.Fatalf("mixed vector result is %T, want []float64", row["v"])
				}
			}
		})
	}
}
