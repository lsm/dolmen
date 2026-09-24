package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func BenchmarkInsertThousandRecords(b *testing.B) {
	st, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	l := legacy(st)
	ctx := context.Background()
	if err := l.CreateNamespace("bench"); err != nil {
		b.Fatal(err)
	}
	if _, err := l.CreateTable(ctx, "bench", "notes", []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true},
		{Name: "body", Type: schema.Text, Fulltext: true},
		{Name: "score", Type: schema.Number},
	}); err != nil {
		b.Fatal(err)
	}
	recs := make([]map[string]any, 1000)
	for i := range recs {
		recs[i] = map[string]any{"title": fmt.Sprintf("note %d", i), "body": "a body with a few words in it", "score": i}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Insert(ctx, "bench", "notes", recs, testEmbed); err != nil {
			b.Fatal(err)
		}
	}
}
