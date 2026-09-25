package store

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func seedVectors(b *testing.B, rows, dim int) *Store {
	return seedVectorsAs(b, rows, dim, false)
}

func seedVectorsAs(b *testing.B, rows, dim int, own bool) *Store {
	b.Helper()
	st, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	l := legacy(st)
	ctx := context.Background()
	if err := l.CreateNamespace("v"); err != nil {
		b.Fatal(err)
	}
	opts := TableOpts{}
	if own {
		opts.RowAccess = schema.RowAccessOwn
	}
	if _, err := st.CreateTable(ctx, "v", "t", []schema.Field{{Name: "title", Type: schema.String}, {Name: "emb", Type: schema.Vector, Dim: dim}}, opts, [16]byte{}); err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	for start := 0; start < rows; start += 1000 {
		batch := make([]map[string]any, 0, 1000)
		for i := start; i < start+1000 && i < rows; i++ {
			v := make([]float32, dim)
			for j := range v {
				v[j] = rng.Float32()*2 - 1
			}
			batch = append(batch, map[string]any{"title": fmt.Sprint(i), "emb": v})
		}
		if _, err := st.Insert(ctx, "v", "t", batch, WriteOpts{Owner: fmt.Sprint("user", start/1000%10)}, testEmbed, nil, Incarnation{}); err != nil {
			b.Fatal(err)
		}
	}
	return st
}

func benchVectorSearch(b *testing.B, rows, dim int) {
	benchVectorQuery(b, seedVectors(b, rows, dim), dim, "", nil)
}

func benchVectorQuery(b *testing.B, st *Store, dim int, filter string, args []any) {
	benchVectorScoped(b, st, dim, filter, args, nil)
}

func benchVectorScoped(b *testing.B, st *Store, dim int, filter string, args []any, scope *RowScope) {
	q := make([]float32, dim)
	for i := range q {
		q[i] = float32(i%7) - 3
	}
	ctx := context.Background()
	if _, err := st.SearchVector(ctx, "v", "t", VectorQuery{Column: "emb", Vec: q, Filter: filter, Args: args}, false, scope, Incarnation{}, Page{Limit: 10}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.SearchVector(ctx, "v", "t", VectorQuery{Column: "emb", Vec: q, Filter: filter, Args: args}, false, scope, Incarnation{}, Page{Limit: 10}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchVector20kx384(b *testing.B)  { benchVectorSearch(b, 20000, 384) }
func BenchmarkSearchVector20kx1536(b *testing.B) { benchVectorSearch(b, 20000, 1536) }

func BenchmarkSearchVector100kx384(b *testing.B) { benchVectorSearch(b, 100000, 384) }

func BenchmarkSearchVector20kx384Filtered(b *testing.B) {
	benchVectorQuery(b, seedVectors(b, 20000, 384), 384, "CAST(title AS INTEGER) % 2 = ?", []any{0})
}

func BenchmarkSearchVector20kx384FilteredNoCache(b *testing.B) {
	st := seedVectors(b, 20000, 384)
	st.vcache.max = 0
	benchVectorQuery(b, st, 384, "CAST(title AS INTEGER) % 2 = ?", []any{0})
}

func BenchmarkSearchVector20kx384Own(b *testing.B) {
	benchVectorScoped(b, seedVectorsAs(b, 20000, 384, true), 384, "", nil, &RowScope{Owner: "user3"})
}

func BenchmarkSearchVector20kx384OwnNoCache(b *testing.B) {
	st := seedVectorsAs(b, 20000, 384, true)
	st.vcache.max = 0
	benchVectorScoped(b, st, 384, "", nil, &RowScope{Owner: "user3"})
}
