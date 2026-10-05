package conformance

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseSearchEngine interface {
	lakehouseReadEngine
	SearchFulltext(context.Context, string, string, string, string, []any, bool, *store.RowScope, store.Incarnation, store.Page) (store.SearchResult, error)
	SearchVector(context.Context, string, string, store.VectorQuery, bool, *store.RowScope, store.Incarnation, store.Page) (store.SearchResult, error)
	Tokenize(context.Context, string, string, string, store.Incarnation) ([]string, error)
}

func searchIDs(rows []map[string]any) []int64 {
	out := []int64{}
	for _, r := range rows {
		out = append(out, r["id"].(int64))
	}
	return out
}

func TestLakehouseSearchBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			key, err := secret.New(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			var raw namespaceEngine
			if backend == "lakehouse" {
				raw, err = lakehouse.Open(t.TempDir(), lakehouse.WithSecretKeyring(key), lakehouse.WithSQLEngine(lakehouseSQLEngine(t)))
			} else {
				raw, err = store.Open(t.TempDir(), store.WithSecretKey(key))
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			eng, ok := raw.(lakehouseSearchEngine)
			if !ok {
				t.Fatal("engine has no search path")
			}
			ctx := t.Context()
			ns := "project"
			none := store.Incarnation{}
			if err := eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{
				{Name: "body", Type: schema.Text, Fulltext: true},
				{Name: "kind", Type: schema.String},
				{Name: "v", Type: schema.Vector, Dim: 2},
			}
			if _, err := eng.CreateTable(ctx, ns, "docs", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, ns, "docs", []map[string]any{
				{"body": "the quick brown fox jumps", "kind": "a", "v": []any{1, 0}},
				{"body": "a lazy dog sleeps all day", "kind": "b", "v": []any{0, 1}},
				{"body": "brown dogs and quick foxes running", "kind": "a", "v": []any{0.7, 0.7}},
			}, store.WriteOpts{}, store.Embedder{}, nil, none); err != nil {
				t.Fatal(err)
			}

			ft := func(match, filter string, page store.Page) []int64 {
				t.Helper()
				res, err := eng.SearchFulltext(ctx, ns, "docs", match, filter, nil, false, nil, none, page)
				if err != nil {
					t.Fatalf("search %q: %v", match, err)
				}
				for _, r := range res.Rows {
					if _, ok := r["_score"].(float64); !ok {
						t.Fatalf("each hit carries a numeric _score: %v", r)
					}
				}
				return searchIDs(res.Rows)
			}
			sorted := func(ids []int64) []int64 {
				out := append([]int64{}, ids...)
				for i := range out {
					for j := i + 1; j < len(out); j++ {
						if out[j] < out[i] {
							out[i], out[j] = out[j], out[i]
						}
					}
				}
				return out
			}
			if got := sorted(ft("fox", "", store.Page{})); !reflect.DeepEqual(got, []int64{1, 3}) {
				t.Fatalf("stemmed term fox = %v", got)
			}
			if got := ft("\"lazy dog\"", "", store.Page{}); !reflect.DeepEqual(got, []int64{2}) {
				t.Fatalf("phrase = %v", got)
			}
			if got := sorted(ft("brown OR sleeps", "", store.Page{})); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
				t.Fatalf("OR = %v", got)
			}
			if got := ft("brown NOT jumps", "", store.Page{}); !reflect.DeepEqual(got, []int64{3}) {
				t.Fatalf("NOT = %v", got)
			}
			if got := ft("qui*", "", store.Page{}); len(got) != 2 {
				t.Fatalf("prefix = %v", got)
			}
			if got := ft("brown", "kind = 'a' AND id > 1", store.Page{}); !reflect.DeepEqual(got, []int64{3}) {
				t.Fatalf("filtered = %v", got)
			}
			if got := ft("zebra", "", store.Page{}); len(got) != 0 {
				t.Fatalf("no hit = %v", got)
			}
			res, err := eng.SearchFulltext(ctx, ns, "docs", "brown", "", nil, false, nil, none, store.Page{Limit: 1})
			if err != nil || len(res.Rows) != 1 || !res.Truncated {
				t.Fatalf("a page short of the hits is truncated: %+v %v", res, err)
			}

			vr, err := eng.SearchVector(ctx, ns, "docs", store.VectorQuery{Column: "v", Vec: []float32{1, 0}}, false, nil, none, store.Page{})
			if err != nil {
				t.Fatal(err)
			}
			if got := searchIDs(vr.Rows); !reflect.DeepEqual(got, []int64{1, 3, 2}) {
				t.Fatalf("vector ranking = %v", got)
			}
			min := 0.5
			vr, err = eng.SearchVector(ctx, ns, "docs", store.VectorQuery{Column: "v", Vec: []float32{1, 0}, MinScore: &min, Filter: "kind = ?", Args: []any{"a"}}, false, nil, none, store.Page{})
			if err != nil || !reflect.DeepEqual(searchIDs(vr.Rows), []int64{1, 3}) {
				t.Fatalf("vector with min_score and filter = %+v %v", vr, err)
			}
			if _, err := eng.SearchVector(ctx, ns, "docs", store.VectorQuery{Column: "v", Vec: []float32{1, 0, 0}}, false, nil, none, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("a wrong-dimension query must be refused: %v", err)
			}

			toks, err := eng.Tokenize(ctx, ns, "docs", "The Running foxes", none)
			if err != nil || len(toks) == 0 {
				t.Fatalf("tokenize = %v %v", toks, err)
			}
			if _, err := eng.CreateTable(ctx, ns, "plain", []schema.Field{{Name: "x", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.SearchFulltext(ctx, ns, "plain", "x", "", nil, false, nil, none, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("a table with no fulltext field cannot be searched: %v", err)
			}
		})
	}
}
