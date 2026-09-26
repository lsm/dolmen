package postgres

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/telemetry/dbstat"
)

func poolGauges(t *testing.T, r *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != dbstat.PoolConnections && m.Name != dbstat.PoolMax {
				continue
			}
			if m.Unit != "{connection}" {
				t.Errorf("%s unit = %q, want {connection}", m.Name, m.Unit)
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				pool, ok := dp.Attributes.Value("db.client.connection.pool.name")
				if !ok || pool.Emit() == "" {
					t.Errorf("%s has no pool name: %s", m.Name, dp.Attributes.Encoded(attribute.DefaultEncoder()))
				}
				state, _ := dp.Attributes.Value("db.client.connection.state")
				if m.Name == dbstat.PoolMax && state.Emit() != "" {
					t.Errorf("%s carries a state attribute: %s", m.Name, state.Emit())
				}
				key := m.Name + "|" + state.Emit()
				if prev, seen := out[key]; seen {
					t.Fatalf("%s was recorded twice: %d then %d", key, prev, dp.Value)
				}
				out[key] = dp.Value
			}
		}
	}
	return out
}

func TestPostgresReportsItsConnectionPoolAsGauges(t *testing.T) {
	r := sdkmetric.NewManualReader()
	cfg := testConfig(t)
	cfg.MaxConns = 4
	cfg.MeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	s := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "pool", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "pool", "t", []schema.Field{{Name: "k", Type: schema.Number}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "pool", "t", []map[string]any{{"k": 1}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	held, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := poolGauges(t, r)
	if got[dbstat.PoolMax+"|"] != 4 {
		t.Errorf("%s = %d, want the configured 4", dbstat.PoolMax, got[dbstat.PoolMax+"|"])
	}
	if got[dbstat.PoolConnections+"|used"] < 1 {
		t.Errorf("%s idle/used = %v while a connection is held", dbstat.PoolConnections, got)
	}
	if idle, used := got[dbstat.PoolConnections+"|idle"], got[dbstat.PoolConnections+"|used"]; idle+used > 4 {
		t.Errorf("idle %d + used %d is over the pool's own maximum of 4", idle, used)
	}
	held.Release()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := poolGauges(t, r); len(got) > 0 {
		t.Errorf("a closed store still reports %v", got)
	}
}

func TestPostgresGaugesNameNoNamespaceOrTable(t *testing.T) {
	r := sdkmetric.NewManualReader()
	cfg := testConfig(t)
	cfg.MeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	s := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "private-namespace", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "private-namespace", "private_table", []schema.Field{{Name: "k", Type: schema.Number}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := r.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				found++
				for _, kv := range dp.Attributes.ToSlice() {
					if v := kv.Value.Emit(); strings.Contains(v, "private") {
						t.Errorf("%s leaks %s=%q into a gauge attribute", m.Name, kv.Key, v)
					}
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("the pool gauges must be recorded for this test to mean anything")
	}
}
