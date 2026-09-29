package inprocess

import (
	"fmt"
	"testing"
	"time"
)

func benchNS(b *testing.B, rows int) *Namespace {
	ns := NewNamespace("bench", Options{ReadOnly: true, Locked: true})
	data := make([][]any, 0, rows)
	for i := 0; i < rows; i++ {
		data = append(data, []any{int64(i), fmt.Sprintf("row %d body", i), float64(i) * 1.5, int8(i % 2)})
	}
	if err := ns.CreateTable(TableSpec{
		Name:    "notes",
		Columns: []Column{{Name: "id", Type: Int64()}, {Name: "body", Type: Text()}, {Name: "score", Type: Float64()}, {Name: "flag", Type: Bool()}},
		Rows:    data,
	}); err != nil {
		b.Fatal(err)
	}
	return ns
}

func BenchmarkScan1000(b *testing.B) {
	useConfinedEngineB(b)
	ns := benchNS(b, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ns.Query("SELECT id, body FROM notes"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanWithFilter1000(b *testing.B) {
	useConfinedEngineB(b)
	ns := benchNS(b, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ns.Query("SELECT id, body FROM notes WHERE id = 500"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScan10000(b *testing.B) {
	useConfinedEngineB(b)
	ns := benchNS(b, 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ns.Query("SELECT id, body FROM notes"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFirstQueryOnAColdEngine(b *testing.B) {
	useConfinedEngineB(b)
	ns := benchNS(b, 100)
	queries := []string{"SELECT 1 AS a", "SELECT id FROM notes LIMIT 1", "SELECT count(*) FROM notes"}
	for _, q := range queries {
		b.Run(q, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				start := time.Now()
				if _, err := ns.Query(q); err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(time.Since(start).Microseconds()), "us/first")
			}
		})
	}
}

func useConfinedEngineB(b *testing.B) {
	b.Cleanup(resetEngine)
}
