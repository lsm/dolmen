package lakehouse

import (
	"testing"

	"github.com/lsm/dolmen/internal/telemetry/dbstat"
)

func TestTheLakehouseReportsItsEngineGauges(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	for _, ns := range []string{"a", "b"} {
		if err := s.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListTables(ctx, ns, nil); err != nil {
			t.Fatal(err)
		}
	}
	var engine dbstat.Engine = s
	snap := engine.TelemetryGauges()
	if got := snap.Value(dbstat.OpenNamespaces); got != 2 {
		t.Errorf("open namespaces = %d, want 2", got)
	}
	if snap.Value(dbstat.DBBytes) <= 0 {
		t.Errorf("catalog bytes = %d, want the open catalogs' size", snap.Value(dbstat.DBBytes))
	}
	if err := s.DropNamespace(ctx, "b", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if got := engine.TelemetryGauges().Value(dbstat.OpenNamespaces); got != 1 {
		t.Errorf("open namespaces after a drop = %d, want 1", got)
	}
}
