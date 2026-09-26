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
