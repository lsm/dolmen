package dbstat_test

import (
	"context"
	"sort"
	"testing"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lsm/dolmen/internal/telemetry/dbstat"
)

type fakeEngine struct{ snap dbstat.Snapshot }

func (f fakeEngine) TelemetryGauges() dbstat.Snapshot { return f.snap }

type point struct {
	attrs map[string]string
	value int64
}

func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func pointsOf(t *testing.T, m metricdata.Metrics) []point {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
	}
	if sum.IsMonotonic {
		t.Errorf("%s must be an up-down counter, not a monotonic one", m.Name)
	}
	var out []point
	for _, dp := range sum.DataPoints {
		attrs := map[string]string{}
		for _, kv := range dp.Attributes.ToSlice() {
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		out = append(out, point{attrs: attrs, value: dp.Value})
	}
	return out
}

func wantGauge(t *testing.T, metrics map[string]metricdata.Metrics, name string, want map[string]int64) {
	t.Helper()
	m, ok := metrics[name]
	if !ok {
		var names []string
		for n := range metrics {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("%s was not recorded; recorded %v", name, names)
	}
	got := pointsOf(t, m)
	if len(got) != len(want) {
		t.Fatalf("%s has %d data points, want %d: %v", name, len(got), len(want), got)
	}
	for _, dp := range got {
		value, ok := want[dp.attrs["db.client.connection.state"]+dp.attrs["db.client.connection.pool.name"]]
		if !ok {
			t.Fatalf("%s has an unexpected data point %v", name, dp.attrs)
		}
		if dp.value != value {
			t.Errorf("%s%v = %d, want %d", name, dp.attrs, dp.value, value)
		}
	}
}

func TestEngineGaugesAreObservedWithBoundedAttributes(t *testing.T) {
	r := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	var snap dbstat.Snapshot
	snap.Pool = "dolmen_catalog"
	snap.Values[dbstat.OpenNamespaces] = 3
	snap.Values[dbstat.VectorCacheUsed] = 4096
	snap.Values[dbstat.VectorCacheMax] = 1 << 20
	snap.Values[dbstat.PoolIdle] = 2
	snap.Values[dbstat.PoolUsed] = 1
	snap.Values[dbstat.PoolLimit] = 8
	stop, err := dbstat.Observe(mp, fakeEngine{snap: snap})
	if err != nil {
		t.Fatal(err)
	}
	if stop == nil {
		t.Fatal("Observe must return a way to unregister the callback")
	}

	metrics := collect(t, r)
	wantGauge(t, metrics, dbstat.NamespacesOpen, map[string]int64{"": 3})
	wantGauge(t, metrics, dbstat.VectorCacheUsage, map[string]int64{"": 4096})
	wantGauge(t, metrics, dbstat.VectorCacheLimit, map[string]int64{"": 1 << 20})
	wantGauge(t, metrics, dbstat.PoolConnections, map[string]int64{
		"idledolmen_catalog": 2,
		"useddolmen_catalog": 1,
	})
	wantGauge(t, metrics, dbstat.PoolMax, map[string]int64{"dolmen_catalog": 8})
	for name, want := range map[string]map[string]int{
		dbstat.NamespacesOpen:   {},
		dbstat.VectorCacheUsage: {},
		dbstat.VectorCacheLimit: {},
		dbstat.PoolMax:          {"db.client.connection.pool.name": 1},
		dbstat.PoolConnections:  {"db.client.connection.pool.name": 1, "db.client.connection.state": 1},
	} {
		for _, dp := range pointsOf(t, metrics[name]) {
			for k := range want {
				if _, ok := dp.attrs[k]; !ok {
					t.Errorf("%s must carry %s; attributes are %v", name, k, dp.attrs)
				}
			}
			if len(dp.attrs) != len(want) {
				t.Errorf("%s carries %d attributes, want %d: %v", name, len(dp.attrs), len(want), dp.attrs)
			}
		}
	}
	for name, want := range map[string]string{
		dbstat.NamespacesOpen:   "{namespace}",
		dbstat.VectorCacheUsage: "By",
		dbstat.VectorCacheLimit: "By",
		dbstat.PoolConnections:  "{connection}",
		dbstat.PoolMax:          "{connection}",
	} {
		if got := metrics[name].Unit; got != want {
			t.Errorf("%s unit = %q, want %q", name, got, want)
		}
	}

	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name := range map[string]int{
		dbstat.NamespacesOpen: 0, dbstat.VectorCacheUsage: 0, dbstat.VectorCacheLimit: 0,
		dbstat.PoolConnections: 0, dbstat.PoolMax: 0,
	} {
		if m, ok := collect(t, r)[name]; ok && len(pointsOf(t, m)) > 0 {
			t.Errorf("%s is still recorded after the callback was unregistered", name)
		}
	}
}

func TestEngineGaugesAreSkippedWithoutAMeterProvider(t *testing.T) {
	engine := fakeEngine{}
	for _, tc := range []struct {
		what string
		mp   metric.MeterProvider
		eng  any
	}{
		{"no meter provider", nil, engine},
		{"a no-op meter provider", noop.NewMeterProvider(), engine},
		{"an engine that reports no gauges", sdkmetric.NewMeterProvider(), "not an engine"},
	} {
		stop, err := dbstat.Observe(tc.mp, tc.eng)
		if err != nil || stop != nil {
			t.Errorf("%s: stop=%v err=%v", tc.what, stop == nil, err)
		}
	}
}

func TestTheMeterProviderTravelsInTheContext(t *testing.T) {
	mp := sdkmetric.NewMeterProvider()
	if dbstat.MeterFrom(context.Background()) != nil {
		t.Fatal("a context without a meter provider must yield nil")
	}
	if dbstat.ContextWithMeter(context.Background(), nil) != context.Background() {
		t.Fatal("a nil meter provider must leave the context alone")
	}
	if got := dbstat.MeterFrom(dbstat.ContextWithMeter(context.Background(), mp)); got != mp {
		t.Fatalf("the meter provider must survive the context, got %v", got)
	}
}
