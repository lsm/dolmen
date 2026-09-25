package store

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func seedVectors(b *testing.B, rows, dim int) *Store {
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
	if _, err := l.CreateTable(ctx, "v", "t", []schema.Field{{Name: "title", Type: schema.String}, {Name: "emb", Type: schema.Vector, Dim: dim}}); err != nil {
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
		if _, err := l.Insert(ctx, "v", "t", batch, testEmbed); err != nil {
			b.Fatal(err)
		}
	}
	return st
}

func benchVectorSearch(b *testing.B, rows, dim int) {
	st := seedVectors(b, rows, dim)
	q := make([]float32, dim)
	for i := range q {
		q[i] = float32(i%7) - 3
	}
	ctx := context.Background()
	if _, err := st.SearchVector(ctx, "v", "t", VectorQuery{Column: "emb", Vec: q}, false, nil, Incarnation{}, Page{Limit: 10}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.SearchVector(ctx, "v", "t", VectorQuery{Column: "emb", Vec: q}, false, nil, Incarnation{}, Page{Limit: 10}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchVector20kx384(b *testing.B)  { benchVectorSearch(b, 20000, 384) }
func BenchmarkSearchVector20kx1536(b *testing.B) { benchVectorSearch(b, 20000, 1536) }

func BenchmarkSearchVector100kx384(b *testing.B) { benchVectorSearch(b, 100000, 384) }
