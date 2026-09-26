package dolmen

import (
	"context"
	"errors"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestWithMeterProviderRecordsOperationMetrics(t *testing.T) {
	r := sdkmetric.NewManualReader()
	st, err := Open(t.TempDir(), WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "app", "notes", []Field{{Name: "body", Type: String}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{{"body": "hello"}}, InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := r.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "dolmen.operation.duration" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				if v, _ := dp.Attributes.Value("dolmen.op.name"); v.AsString() == "insert" {
					return
				}
			}
		}
	}
	t.Fatal("the library must record an insert in dolmen.operation.duration")
}

func TestWithMeterProviderRefusesNil(t *testing.T) {
	_, err := Open(t.TempDir(), WithMeterProvider(nil))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a nil meter provider must be refused as invalid_request, got %v", err)
	}
}

func TestWithMeterProviderRecordsTheEngineCapacityGauges(t *testing.T) {
	r := sdkmetric.NewManualReader()
	st, err := Open(t.TempDir(), WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "app", "notes", []Field{{Name: "body", Type: String}}); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := r.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "dolmen.namespaces.open" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("dolmen.namespaces.open is %T, want an int64 sum", m.Data)
			}
			if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 {
				t.Fatalf("dolmen.namespaces.open = %v, want one open namespace", sum.DataPoints)
			}
			if n := sum.DataPoints[0].Attributes.Len(); n != 0 {
				t.Errorf("dolmen.namespaces.open carries %d attributes; a gauge must not name a namespace", n)
			}
			return
		}
	}
	t.Fatal("the library must pass its meter provider to the engine, or dolmen.namespaces.open is never recorded")
}
