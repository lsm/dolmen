package store

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

func BenchmarkNamespaceWrites(b *testing.B) {
	for _, namespaces := range []int{1, 8} {
		for _, callers := range []int{1, 4, 16} {
			for _, rows := range []int{1, 16, 128} {
				b.Run(fmt.Sprintf("ns=%d/callers=%d/rows=%d", namespaces, callers, rows), func(b *testing.B) {
					benchmarkNamespaceWrites(b, namespaces, callers, rows)
				})
			}
		}
	}
}

func benchmarkNamespaceWrites(b *testing.B, namespaces, callers, rows int) {
	ctx := context.Background()
	st, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := st.Close(); err != nil {
			b.Error(err)
		}
	})
	names := make([]string, namespaces)
	for i := range names {
		names[i] = fmt.Sprintf("synthetic%d", i)
		if err := st.CreateNamespace(ctx, names[i], [16]byte{}); err != nil {
			b.Fatal(err)
		}
		if _, err := st.CreateTable(ctx, names[i], "notes", []schema.Field{
			{Name: "body", Type: schema.String},
			{Name: "score", Type: schema.Number},
		}, TableOpts{}, [16]byte{}); err != nil {
			b.Fatal(err)
		}
	}
	latencies := make([]time.Duration, b.N)
	var next atomic.Int64
	var ready, done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	start := make(chan struct{})
	failures := make(chan error, callers)
	for range callers {
		go func() {
			defer done.Done()
			records := make([]map[string]any, rows)
			for i := range records {
				records[i] = map[string]any{"body": strings.Repeat("x", 256), "score": i}
			}
			ready.Done()
			<-start
			for {
				i := int(next.Add(1) - 1)
				if i >= b.N {
					return
				}
				before := time.Now()
				result, err := st.Insert(ctx, names[i%namespaces], "notes", records,
					WriteOpts{}, Embedder{}, nil, Incarnation{})
				latencies[i] = time.Since(before)
				if err != nil {
					failures <- err
					return
				}
				if len(result.Ids) != rows {
					failures <- fmt.Errorf("insert returned %d ids, want %d", len(result.Ids), rows)
					return
				}
			}
		}()
	}
	ready.Wait()
	b.ResetTimer()
	close(start)
	done.Wait()
	b.StopTimer()
	close(failures)
	for err := range failures {
		b.Fatal(err)
	}
	for i, ns := range names {
		_, count, err := st.DescribeTable(ctx, ns, "notes", nil, Incarnation{})
		calls := b.N / namespaces
		if i < b.N%namespaces {
			calls++
		}
		want := int64(calls * rows)
		if err != nil || count != want {
			b.Fatalf("%s: persisted rows %d, want %d: %v", ns, count, want, err)
		}
	}
	slices.Sort(latencies)
	for _, p := range []int{50, 95, 99} {
		i := int(math.Ceil(float64(len(latencies))*float64(p)/100)) - 1
		b.ReportMetric(float64(latencies[i])/float64(time.Millisecond), fmt.Sprintf("p%d-ms", p))
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "calls/s")
	b.ReportMetric(float64(b.N)*float64(rows)/b.Elapsed().Seconds(), "rows/s")
}
