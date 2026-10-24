package main

import (
	"reflect"
	"testing"

	gb "github.com/lsm/dolmen/spike/graph-index"
)

func TestSyntheticFixture(t *testing.T) {
	a, b := fixture(1000, 10000), fixture(1000, 10000)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("fixture changes across backends")
	}
	hubs := 0
	for j, e := range a {
		if e.ID != uint64(j+1) || e.From < 1 || e.From > 1000 || e.To < 1 || e.To > 1000 {
			t.Fatalf("invalid edge: %+v", e)
		}
		if e.From == 1 {
			hubs++
		}
	}
	if hubs < 400 {
		t.Fatal("hub workload disappeared")
	}
	for j := 0; j < 100; j++ {
		q := query(j, 1000, 4, true, false)
		if !q.Visible(q.Seeds[0]) || !q.Visible(q.Target) {
			t.Fatal("path endpoints hidden")
		}
	}
}
func TestMeasurementsCountResults(t *testing.T) {
	r, err := measure("probe", 100, func(int) (gb.Result, error) { return gb.Result{Nodes: []uint64{1}, Scanned: 2, Truncated: true}, nil })
	if err != nil || r.Samples != 100 || r.Truncated != 100 || r.Scanned != 200 || r.OpsPerSecond <= 0 || r.P99MS < r.P50MS {
		t.Fatalf("invalid measurement: %+v %v", r, err)
	}
}
func TestBackendMustBeExplicit(t *testing.T) {
	if _, err := open("", t.TempDir()); err == nil {
		t.Fatal("benchmark must not choose a default")
	}
}
