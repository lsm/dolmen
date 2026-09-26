package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/telemetry"
	"github.com/lsm/dolmen/internal/telemetry/dbstat"
)

func TestTheServerRecordsStorageSpansUnderItsOperations(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	cfg := &config{DataDir: filepath.Join(t.TempDir(), "data"), MaxOpenNamespaces: 8, Sync: "full", VectorCacheSize: 1 << 20}
	st, err := openStore(cfg, tp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := api.New(st, mustNoneProvider(t), api.WithTracing(telemetry.New(tp, nil, false)))
	ctx := context.Background()
	for _, c := range []struct{ op, body string }{
		{"create_namespace", `{"namespace":"t"}`},
		{"create_table", `{"namespace":"t","table":"notes","fields":[{"name":"body","type":"string","fulltext":true}]}`},
		{"insert", `{"namespace":"t","table":"notes","records":[{"body":"hello there"}]}`},
		{"search_fulltext", `{"namespace":"t","table":"notes","query":"hello"}`},
	} {
		if _, err := srv.Dispatch(ctx, c.op, []byte(c.body)); err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
	}
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
		for _, a := range s.Attributes() {
			if strings.Contains(a.Value.Emit(), "hello") {
				t.Errorf("span %s leaks row data in %s", s.Name(), a.Key)
			}
		}
	}
	joined := strings.Join(names, " ")
	for _, want := range []string{"dolmen.op insert", "INSERT", "SELECT"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the server must record %q; recorded %v", want, names)
		}
	}
}

func mustNoneProvider(t *testing.T) embed.Provider {
	t.Helper()
	p, err := embed.NewProvider("none", "", "", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheServerReportsItsEngineCapacityAsMetrics(t *testing.T) {
	r := sdkmetric.NewManualReader()
	cfg := &config{DataDir: filepath.Join(t.TempDir(), "data"), MaxOpenNamespaces: 8, Sync: "full", VectorCacheSize: 1 << 20}
	st, err := openStore(cfg, nil, sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := api.New(st, mustNoneProvider(t))
	ctx := context.Background()
	for _, c := range []struct{ op, body string }{
		{"create_namespace", `{"namespace":"t"}`},
		{"create_table", `{"namespace":"t","table":"notes","fields":[{"name":"body","type":"string"}]}`},
	} {
		if _, err := srv.Dispatch(ctx, c.op, []byte(c.body)); err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
	}
	var rm metricdata.ResourceMetrics
	if err := r.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{dbstat.NamespacesOpen: 1, dbstat.VectorCacheLimit: 1 << 20, dbstat.VectorCacheUsage: 0}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			expect, ok := want[m.Name]
			if !ok {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Errorf("%s is %T, want an int64 sum", m.Name, m.Data)
				continue
			}
			if len(sum.DataPoints) != 1 {
				t.Errorf("%s has %d data points, want 1", m.Name, len(sum.DataPoints))
				continue
			}
			if got := sum.DataPoints[0].Value; got != expect {
				t.Errorf("%s = %d, want %d", m.Name, got, expect)
			}
			if attrs := sum.DataPoints[0].Attributes.Len(); attrs != 0 {
				t.Errorf("%s carries %d attributes; a gauge must not name a namespace or a table", m.Name, attrs)
			}
			delete(want, m.Name)
		}
	}
	for name := range want {
		t.Errorf("%s was not reported; the server must pass its meter provider to the engine", name)
	}
}
