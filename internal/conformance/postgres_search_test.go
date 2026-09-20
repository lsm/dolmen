package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type searchEngine interface {
	insertEngine
	SearchFulltext(context.Context, string, string, string, string, []any, bool, *store.RowScope, store.Incarnation, store.Page) (store.SearchResult, error)
	SearchVector(context.Context, string, string, store.VectorQuery, bool, *store.RowScope, store.Incarnation, store.Page) (store.SearchResult, error)
}

func searchBackendEngine(t *testing.T, backend string) searchEngine {
	t.Helper()
	if backend == "postgres" {
		return postgresNamespaceEngine(t).(searchEngine)
	}
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func resultIDs(t *testing.T, result store.SearchResult) []int64 {
	t.Helper()
	ids := make([]int64, len(result.Rows))
	for i, row := range result.Rows {
		id, ok := row["id"].(int64)
		if !ok {
			t.Fatalf("row %d has no int64 id: %+v", i, row)
		}
		ids[i] = id
	}
	return ids
}

func TestFulltextSearchBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			eng := searchBackendEngine(t, backend)
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{{Name: "title", Fulltext: true}, {Name: "kind"}, {Name: "score", Type: schema.Number}}
			if _, err := eng.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, "app", "notes", []map[string]any{
				{"title": "alpha gateway", "kind": "doc", "score": 1},
				{"title": "beta gateway", "kind": "policy", "score": 2},
				{"title": "gamma unrelated", "kind": "doc", "score": 3},
			}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}

			result, err := eng.SearchFulltext(ctx, "app", "notes", "gateway", "", nil, false, nil, store.Incarnation{}, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Rows) != 2 {
				t.Fatalf("gateway matched %+v", resultIDs(t, result))
			}
			for _, row := range result.Rows {
				if row["title"] == nil || row["kind"] == nil || row["created_at"] == nil {
					t.Fatalf("row shape: %+v", row)
				}
				if _, ok := row["score"].(int64); !ok {
					t.Fatalf("declared number type not honored: %+v", row["score"])
				}
				if _, ok := row["_embedding"]; ok {
					t.Fatalf("hidden column exposed: %+v", row)
				}
			}

			filtered, err := eng.SearchFulltext(ctx, "app", "notes", "gateway", "kind = ?", []any{"policy"}, false, nil, store.Incarnation{}, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			if ids := resultIDs(t, filtered); len(ids) != 1 || ids[0] != 2 {
				t.Fatalf("filter before ranking: %+v", ids)
			}

			page, err := eng.SearchFulltext(ctx, "app", "notes", "gateway", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Rows) != 1 || !page.Truncated {
				t.Fatalf("truncation: rows=%d truncated=%v", len(page.Rows), page.Truncated)
			}
			past, err := eng.SearchFulltext(ctx, "app", "notes", "gateway", "", nil, false, nil, store.Incarnation{}, store.Page{Offset: 5})
			if err != nil {
				t.Fatal(err)
			}
			if len(past.Rows) != 0 || past.Truncated {
				t.Fatalf("past the end: %+v", past)
			}
			if _, err := eng.SearchFulltext(ctx, "app", "notes", "gateway", "", nil, false, nil, store.Incarnation{}, store.Page{Offset: -1}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("negative offset: %v", err)
			}
			if _, err := eng.SearchFulltext(ctx, "app", "notes", "gateway", "kind = ? AND", []any{"doc"}, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("malformed filter: %v", err)
			}
		})
	}
}

func TestFulltextSearchRequiresIndexedFieldBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			eng := searchBackendEngine(t, backend)
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.CreateTable(ctx, "app", "plain", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.SearchFulltext(ctx, "app", "plain", "anything", "", nil, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("table without fulltext fields: %v", err)
			}
		})
	}
}

func TestVectorSearchBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			eng := searchBackendEngine(t, backend)
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{{Name: "kind"}, {Name: "vec", Type: schema.Vector, Dim: 3}}
			if _, err := eng.CreateTable(ctx, "app", "points", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, "app", "points", []map[string]any{
				{"kind": "a", "vec": []any{1.0, 0.0, 0.0}},
				{"kind": "b", "vec": []any{1.0, 1.0, 0.0}},
				{"kind": "a", "vec": []any{0.0, 1.0, 0.0}},
				{"kind": "a"},
			}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}

			result, err := eng.SearchVector(ctx, "app", "points", store.VectorQuery{Vec: []float32{1, 0, 0}}, false, nil, store.Incarnation{}, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			if ids := resultIDs(t, result); len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
				t.Fatalf("cosine ordering: %+v", ids)
			}
			if result.Execution != store.VectorExact {
				t.Fatalf("execution: %q", result.Execution)
			}
			if result.SkippedVectors != 0 {
				t.Fatalf("skipped: %d", result.SkippedVectors)
			}
			for _, row := range result.Rows {
				if _, ok := row["_score"].(float64); !ok {
					t.Fatalf("row without _score: %+v", row)
				}
			}
			if top := result.Rows[0]["_score"].(float64); top < 0.999 {
				t.Fatalf("top score %v", top)
			}

			min := 0.9
			thresholded, err := eng.SearchVector(ctx, "app", "points", store.VectorQuery{Vec: []float32{1, 0, 0}, MinScore: &min}, false, nil, store.Incarnation{}, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			if ids := resultIDs(t, thresholded); len(ids) != 1 || ids[0] != 1 {
				t.Fatalf("min_score: %+v", ids)
			}

			filtered, err := eng.SearchVector(ctx, "app", "points", store.VectorQuery{Vec: []float32{1, 0, 0}, Filter: "kind = ?", Args: []any{"a"}}, false, nil, store.Incarnation{}, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			if ids := resultIDs(t, filtered); len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
				t.Fatalf("filtered: %+v", ids)
			}

			page, err := eng.SearchVector(ctx, "app", "points", store.VectorQuery{Vec: []float32{1, 0, 0}}, false, nil, store.Incarnation{}, store.Page{Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Rows) != 2 || !page.Truncated {
				t.Fatalf("truncation: rows=%d truncated=%v", len(page.Rows), page.Truncated)
			}
			next, err := eng.SearchVector(ctx, "app", "points", store.VectorQuery{Vec: []float32{1, 0, 0}}, false, nil, store.Incarnation{}, store.Page{Limit: 2, Offset: 2})
			if err != nil {
				t.Fatal(err)
			}
			if ids := resultIDs(t, next); len(ids) != 1 || ids[0] != 3 {
				t.Fatalf("second page: %+v", ids)
			}
			if _, err := eng.SearchVector(ctx, "app", "points", store.VectorQuery{Vec: []float32{1, 0}}, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("dimension mismatch: %v", err)
			}
		})
	}
}
