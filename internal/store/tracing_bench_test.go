package store

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/lsm/dolmen/internal/schema"
)

func tracingBenchModes() map[string][]OpenOption {
	sampledOut := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	return map[string][]OpenOption{
		"off":         nil,
		"sampled_out": {WithTracerProvider(sampledOut)},
	}
}

func BenchmarkTracingInsertOne(b *testing.B) {
	for name, opts := range tracingBenchModes() {
		b.Run(name, func(b *testing.B) {
			st, err := Open(b.TempDir(), opts...)
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			if err := st.CreateNamespace(ctx, "bench", [16]byte{}); err != nil {
				b.Fatal(err)
			}
			if _, err := st.CreateTable(ctx, "bench", "notes", []schema.Field{{Name: "title", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
				b.Fatal(err)
			}
			rec := []map[string]any{{"title": "note"}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := st.Insert(ctx, "bench", "notes", rec, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkTracingSearchVector1kx64(b *testing.B) {
	for name, opts := range tracingBenchModes() {
		b.Run(name, func(b *testing.B) {
			st, err := Open(b.TempDir(), opts...)
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			if err := st.CreateNamespace(ctx, "v", [16]byte{}); err != nil {
				b.Fatal(err)
			}
			if _, err := st.CreateTable(ctx, "v", "t", []schema.Field{{Name: "emb", Type: schema.Vector, Dim: 64}}, TableOpts{}, [16]byte{}); err != nil {
				b.Fatal(err)
			}
			batch := make([]map[string]any, 1000)
			for i := range batch {
				v := make([]float32, 64)
				v[i%64] = 1
				batch[i] = map[string]any{"emb": v}
			}
			if _, err := st.Insert(ctx, "v", "t", batch, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
				b.Fatal(err)
			}
			q := make([]float32, 64)
			q[3] = 1
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := st.SearchVector(ctx, "v", "t", VectorQuery{Column: "emb", Vec: q}, false, nil, Incarnation{}, Page{Limit: 10}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
