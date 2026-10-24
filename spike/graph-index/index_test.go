package graphbench_test

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	gb "github.com/lsm/dolmen/spike/graph-index"
	bi "github.com/lsm/dolmen/spike/graph-index/badger"
	si "github.com/lsm/dolmen/spike/graph-index/sqlite"
)

var factories = map[string]func(string) (gb.Index, error){"sqlite": si.Open, "badger": bi.Open}
var fixture = []gb.Edge{{1, 1, 2, 1}, {2, 2, 3, 1}, {3, 3, 1, 1}, {4, 1, 4, 2}, {5, 4, 3, 2}}

func TestBackendContracts(t *testing.T) {
	for name, open := range factories {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			db, err := open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if db != nil {
					db.Close()
				}
			}()
			if err := db.Append(ctx, fixture); err != nil {
				t.Fatal(err)
			}
			view, err := db.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got, more, err := view.Adjacent(ctx, 1, false, 1)
			if err != nil || !more || !reflect.DeepEqual(got, fixture[:1]) {
				t.Fatalf("bounded adjacency: %v %v %v", got, more, err)
			}
			if err := db.Append(ctx, []gb.Edge{{6, 1, 5, 1}}); err != nil {
				t.Fatal(err)
			}
			got, _, err = view.Adjacent(ctx, 1, false, 10)
			if err != nil || len(got) != 2 {
				t.Fatalf("snapshot changed: %v %v", got, err)
			}
			view.Close()
			if err := db.Append(ctx, []gb.Edge{{7, 1, 6, 1}, {6, 2, 6, 1}}); err == nil {
				t.Fatal("duplicate ID must roll back batch")
			}
			view, err = db.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got, _, err = view.Adjacent(ctx, 1, false, 10)
			if err != nil || len(got) != 3 {
				t.Fatalf("partial append: %v %v", got, err)
			}
			got, _, err = view.Adjacent(ctx, 3, true, 10)
			if err != nil || len(got) != 2 || got[0].From != 2 || got[1].From != 4 {
				t.Fatalf("reverse ordering: %v %v", got, err)
			}
			view.Close()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = nil
			db, err = open(dir)
			if err != nil {
				t.Fatal(err)
			}
			view, err = db.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
			got, _, err = view.Adjacent(ctx, 1, false, 10)
			if err != nil || len(got) != 3 {
				t.Fatalf("reopen lost commit: %v %v", got, err)
			}
		})
	}
}

func TestBoundedTraversalParity(t *testing.T) {
	for name, open := range factories {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, err := open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.Append(ctx, fixture); err != nil {
				t.Fatal(err)
			}
			q := gb.Query{Seeds: []uint64{1}, Target: 3, Hops: 4, Nodes: 10, Edges: 50, Elapsed: time.Second}
			run := func(q gb.Query) gb.Result {
				t.Helper()
				v, err := db.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer v.Close()
				r, err := gb.Traverse(ctx, v, q)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			if r := run(q); !reflect.DeepEqual(r.Path, []uint64{1, 2, 3}) {
				t.Fatalf("shortest stable path: %+v", r)
			}
			q.Visible = func(n uint64) bool { return n != 2 && n != 4 }
			if r := run(q); len(r.Path) != 0 || !reflect.DeepEqual(r.Nodes, []uint64{1}) {
				t.Fatalf("hidden bridge: %+v", r)
			}
			q.Visible = nil
			q.EdgeVisible = func(e gb.Edge) bool { return e.ID != 1 && e.ID != 4 }
			if r := run(q); len(r.Path) != 0 {
				t.Fatalf("hidden edge bridge: %+v", r)
			}
			q.EdgeVisible = nil
			q.Target = 0
			if r := run(q); len(r.Nodes) != 4 || len(r.EdgeIDs) != 5 || r.Truncated {
				t.Fatalf("cycle: %+v", r)
			}
			q.Nodes = 2
			if r := run(q); !r.Truncated || r.Stop != "nodes" {
				t.Fatalf("node cap: %+v", r)
			}
			q.Nodes = 10
			q.Edges = 1
			if r := run(q); !r.Truncated || r.Scanned > 1 {
				t.Fatalf("edge cap: %+v", r)
			}
			q.Edges = 1
			q.Target = 2
			if r := run(q); !reflect.DeepEqual(r.Path, []uint64{1, 2}) || r.Truncated {
				t.Fatalf("path at edge budget: %+v", r)
			}
			q.Target = 0
			q.Edges = 50
			q.Elapsed = time.Nanosecond
			if r := run(q); !r.Truncated || r.Stop != "elapsed" {
				t.Fatalf("elapsed cap: %+v", r)
			}
			q.Elapsed = time.Second
			q.Kind = 2
			if r := run(q); !reflect.DeepEqual(r.Nodes, []uint64{1, 3, 4}) {
				t.Fatalf("kind filter: %+v", r)
			}
			q.Kind = 0
			q.Target = 3
			q.Hops = 1
			if r := run(q); len(r.Path) != 0 || !r.Truncated {
				t.Fatalf("bounded miss: %+v", r)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			v, err := db.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if _, err := gb.Traverse(canceled, v, q); err == nil {
				t.Fatal("cancellation ignored")
			}
		})
	}
}

func TestAbruptExitRecovery(t *testing.T) {
	if backend := os.Getenv("GRAPH_CRASH_BACKEND"); backend != "" {
		db, err := factories[backend](os.Getenv("GRAPH_CRASH_DIR"))
		if err != nil {
			os.Exit(2)
		}
		if err := db.Append(context.Background(), fixture); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	for name, open := range factories {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestAbruptExitRecovery$")
			cmd.Env = append(os.Environ(), "GRAPH_CRASH_BACKEND="+name, "GRAPH_CRASH_DIR="+dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child: %v %s", err, out)
			}
			db, err := open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			v, err := db.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			edges, _, err := v.Adjacent(context.Background(), 1, false, 10)
			if err != nil || len(edges) != 2 {
				t.Fatalf("lost acknowledged append: %v %v", edges, err)
			}
		})
	}
}
