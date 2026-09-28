package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/telemetry/dbstat"
)

type gauge struct {
	value  int64
	attrs  map[string]string
	found  bool
	unit   string
	metric string
}

func (g gauge) has(key, want string) bool { return g.attrs[key] == want }

func readGauge(t *testing.T, r *sdkmetric.ManualReader, name string) gauge {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out gauge
	out.metric = name
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
			}
			if sum.IsMonotonic {
				t.Errorf("%s must be an up-down counter", name)
			}
			out.unit = m.Unit
			if len(sum.DataPoints) != 1 {
				t.Fatalf("%s has %d data points, want exactly 1", name, len(sum.DataPoints))
			}
			dp := sum.DataPoints[0]
			out.found = true
			out.value = dp.Value
			out.attrs = map[string]string{}
			for _, kv := range dp.Attributes.ToSlice() {
				out.attrs[string(kv.Key)] = kv.Value.Emit()
			}
		}
	}
	return out
}

func TestTheStoreReportsOpenNamespacesAndTheVectorCache(t *testing.T) {
	r := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	st, err := Open(t.TempDir(), WithMeterProvider(mp), WithVectorCacheBytes(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := legacy(st)
	ctx := context.Background()

	if got := readGauge(t, r, dbstat.NamespacesOpen); !got.found || got.value != 0 {
		t.Fatalf("%s = %+v before any namespace is touched, want 0", dbstat.NamespacesOpen, got)
	}
	for _, ns := range []string{"one", "two"} {
		mustNS(t, l, ns)
		if _, err := l.CreateTable(ctx, ns, "t", []schema.Field{{Name: "k", Type: schema.Number}, {Name: "emb", Type: schema.Vector, Dim: 3}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := readGauge(t, r, dbstat.NamespacesOpen); got.value != 2 {
		t.Errorf("%s = %d, want 2", dbstat.NamespacesOpen, got.value)
	}
	if got := readGauge(t, r, dbstat.VectorCacheUsage); !got.found || got.value != 0 {
		t.Errorf("%s = %+v before a vector search, want 0", dbstat.VectorCacheUsage, got)
	}
	if got := readGauge(t, r, dbstat.VectorCacheLimit); !got.found || got.value != 1<<20 {
		t.Errorf("%s = %+v, want the configured %d", dbstat.VectorCacheLimit, got, 1<<20)
	}
	if got := readGauge(t, r, dbstat.VectorCacheLimit); got.unit != "By" {
		t.Errorf("%s unit = %q, want By", dbstat.VectorCacheLimit, got.unit)
	}
	if got := readGauge(t, r, dbstat.NamespacesOpen); got.unit != "{namespace}" {
		t.Errorf("%s unit = %q, want {namespace}", dbstat.NamespacesOpen, got.unit)
	}
	for _, name := range []string{dbstat.NamespacesOpen, dbstat.VectorCacheUsage, dbstat.VectorCacheLimit} {
		got := readGauge(t, r, name)
		if len(got.attrs) != 0 {
			t.Errorf("%s carries %v; gauges must not name a namespace or a table", name, got.attrs)
		}
	}
	if got := readGauge(t, r, dbstat.PoolConnections); got.found {
		t.Errorf("the SQLite engine has no connection pool, but %s was recorded as %+v", dbstat.PoolConnections, got)
	}

	if _, err := l.Insert(ctx, "one", "t", []map[string]any{{"k": 1, "emb": []any{1.0, 0.0, 0.0}}, {"k": 2, "emb": []any{0.0, 1.0, 0.0}}}, testEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SearchVector(ctx, "one", "t", "emb", []float32{1, 0, 0}, "", 0, 5, false, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := readGauge(t, r, dbstat.VectorCacheUsage); got.value <= 0 {
		t.Errorf("%s = %d after a vector search, want the bytes it cached", dbstat.VectorCacheUsage, got.value)
	}
	if used, limit := readGauge(t, r, dbstat.VectorCacheUsage).value, readGauge(t, r, dbstat.VectorCacheLimit).value; used > limit {
		t.Errorf("%s = %d is over its %s = %d", dbstat.VectorCacheUsage, used, dbstat.VectorCacheLimit, limit)
	}
}

func TestTheStoreStopsReportingGaugesOnceItIsClosed(t *testing.T) {
	r := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	st, err := Open(t.TempDir(), WithMeterProvider(mp))
	if err != nil {
		t.Fatal(err)
	}
	mustNS(t, legacy(st), "t")
	if got := readGauge(t, r, dbstat.NamespacesOpen); !got.found {
		t.Fatal("the store must report its open namespaces before it closes")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dbstat.NamespacesOpen, dbstat.VectorCacheUsage, dbstat.VectorCacheLimit} {
		if got := readGauge(t, r, name); got.found {
			t.Errorf("%s is still reported by a closed store: %+v", name, got)
		}
	}
}

func TestTheStoreNeedsNoMeterProviderToOpen(t *testing.T) {
	st, err := Open(t.TempDir(), WithMeterProvider(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var snap dbstat.Snapshot = st.TelemetryGauges()
	if snap.Value(dbstat.VectorCacheMax) != DefaultVectorCacheBytes {
		t.Errorf("vector cache limit = %d, want %d", snap.Value(dbstat.VectorCacheMax), DefaultVectorCacheBytes)
	}
	if snap.Pool != "" {
		t.Errorf("the SQLite engine has no pool, got %q", snap.Pool)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func TestTheStoreReportsDatabaseAndWALSizeForOpenNamespaces(t *testing.T) {
	r := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	dir := t.TempDir()
	st, err := Open(dir, WithMeterProvider(mp))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := legacy(st)
	ctx := context.Background()
	for _, ns := range []string{"alpha", "beta"} {
		if err := l.CreateNamespace(ns); err != nil {
			t.Fatal(err)
		}
		if _, err := l.CreateTable(ctx, ns, "docs", []schema.Field{{Name: "body", Type: schema.Text}}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 40; i++ {
			if _, err := l.Insert(ctx, ns, "docs", []map[string]any{{"body": fmt.Sprintf("row %d of the size gauge fixture", i)}}, Embedder{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var wantDB, wantWAL, wantLargest int64
	for _, ns := range []string{"alpha", "beta"} {
		base := filepath.Join(dir, ns+".db")
		wantDB += fileSize(t, base)
		size := fileSize(t, base+"-wal")
		wantWAL += size
		if size > wantLargest {
			wantLargest = size
		}
	}
	if wantDB == 0 {
		t.Fatal("the fixture wrote nothing to disk, so the gauge has nothing to match")
	}
	if got := readGauge(t, r, dbstat.DBSize); !got.found || got.unit != "By" || got.value != wantDB {
		t.Errorf("%s = %+v, want %d bytes of database files", dbstat.DBSize, got, wantDB)
	}
	if got := readGauge(t, r, dbstat.WALSize); !got.found || got.unit != "By" || got.value != wantWAL {
		t.Errorf("%s = %+v, want %d bytes of -wal files", dbstat.WALSize, got, wantWAL)
	}
	if got := readGauge(t, r, dbstat.WALLargest); !got.found || got.unit != "By" || got.value != wantLargest {
		t.Errorf("%s = %+v, want the largest single -wal at %d bytes", dbstat.WALLargest, got, wantLargest)
	}
	for _, name := range []string{dbstat.DBSize, dbstat.WALSize, dbstat.WALLargest} {
		if got := readGauge(t, r, name); len(got.attrs) != 0 {
			t.Errorf("%s carries attributes %v, want none: a namespace label would make this a per-namespace gauge and the sum is the point", name, got.attrs)
		}
	}
}

func TestTheSizeGaugesStopCountingANamespaceThatIsNoLongerOpen(t *testing.T) {
	r := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	dir := t.TempDir()
	st, err := Open(dir, WithMeterProvider(mp), WithMaxOpenNamespaces(1))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := legacy(st)
	ctx := context.Background()
	for _, ns := range []string{"kept", "gone"} {
		if err := l.CreateNamespace(ns); err != nil {
			t.Fatal(err)
		}
		if _, err := l.CreateTable(ctx, ns, "docs", []schema.Field{{Name: "body", Type: schema.Text}}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 40; i++ {
			if _, err := l.Insert(ctx, ns, "docs", []map[string]any{{"body": fmt.Sprintf("row %d of the eviction fixture", i)}}, Embedder{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := st.ListTables(ctx, "kept", nil); err != nil {
		t.Fatal(err)
	}
	if got := readGauge(t, r, dbstat.NamespacesOpen); got.value != 1 {
		t.Fatalf("the fixture left %d namespaces open, want 1 so the gauge has something to exclude", got.value)
	}
	keptPath := filepath.Join(dir, "kept.db")
	if fileSize(t, keptPath) == 0 {
		t.Fatal("the open namespace's file is empty, so the gauge has nothing to match")
	}
	if got := readGauge(t, r, dbstat.DBSize); got.value != fileSize(t, keptPath) {
		t.Fatalf("%s = %d, want only the open namespace's %d bytes: the evicted namespace's file is still on disk and must stop counting",
			dbstat.DBSize, got.value, fileSize(t, keptPath))
	}
	if err := l.DropNamespace("kept"); err != nil {
		t.Fatal(err)
	}
	if got := readGauge(t, r, dbstat.DBSize); got.value != 0 {
		t.Fatalf("%s = %d after the last namespace was dropped, want 0", dbstat.DBSize, got.value)
	}
	if got := readGauge(t, r, dbstat.WALSize); got.value != 0 {
		t.Fatalf("%s = %d after the last namespace was dropped, want 0", dbstat.WALSize, got.value)
	}
	if got := readGauge(t, r, dbstat.WALLargest); got.value != 0 {
		t.Fatalf("%s = %d after the last namespace was dropped, want 0", dbstat.WALLargest, got.value)
	}
}
