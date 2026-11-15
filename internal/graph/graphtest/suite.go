package graphtest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lsm/dolmen/internal/graph"
)

type Backend interface {
	Namespace() string
	Index(hidden ...string) graph.Index
	Insert(t *testing.T, table string, rows ...map[string]any) []int64
	Update(t *testing.T, table string, id int64, values map[string]any)
	Delete(t *testing.T, table string, id int64)
	Drop(t *testing.T, table string)
	CreateRelation(t *testing.T, table, source, target string)
}

type fixture struct {
	Backend
	t           *testing.T
	alpha, beta []int64
}

func Run(t *testing.T, open func(*testing.T) Backend) {
	cases := []struct {
		name string
		run  func(fixture)
	}{
		{"forward_reverse_both", forwardReverseBoth},
		{"order_across_tables_and_limit", orderAndLimit},
		{"self_loop_once", selfLoopOnce},
		{"snapshot_isolation_and_read_your_writes", snapshotIsolation},
		{"update_moves_edge", updateMovesEdge},
		{"delete_removes_edge_and_row", deleteRemoves},
		{"visible_hides_absent_and_hidden_rows", visibleRows},
		{"hidden_relation_table", hiddenRelationTable},
		{"dropped_relation_table", droppedRelationTable},
		{"recreated_relation_table_serves_no_old_edges", recreatedRelationTable},
		{"refuses_bad_direction_and_limit", refusesBadInput},
		{"close_is_idempotent", closeIdempotent},
		{"canceled_snapshot", canceledSnapshot},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := open(t)
			f := fixture{Backend: b, t: t}
			f.alpha = b.Insert(t, "alpha", map[string]any{"label": "a1"}, map[string]any{"label": "a2"})
			f.beta = b.Insert(t, "beta", map[string]any{"label": "b1"}, map[string]any{"label": "b2"})
			c.run(f)
		})
	}
}

func (f fixture) a(i int) graph.Ref { return graph.Ref{Table: "alpha", ID: f.alpha[i]} }
func (f fixture) b(i int) graph.Ref { return graph.Ref{Table: "beta", ID: f.beta[i]} }

func (f fixture) link(table string, src, dst graph.Ref, kind string) graph.Edge {
	f.t.Helper()
	id := f.Insert(f.t, table, map[string]any{"src": src.ID, "dst": dst.ID, "kind": kind})[0]
	return graph.Edge{Ref: graph.Ref{Table: table, ID: id}, Source: src, Target: dst, Type: kind}
}

func (f fixture) view(hidden ...string) graph.Snapshot {
	f.t.Helper()
	v, err := f.Index(hidden...).Snapshot(f.t.Context(), f.Namespace())
	if err != nil {
		f.t.Fatalf("snapshot: %v", err)
	}
	f.t.Cleanup(func() { v.Close() })
	return v
}

func (f fixture) adjacent(v graph.Snapshot, node graph.Ref, direction string, limit int, want []graph.Edge, wantMore bool) {
	f.t.Helper()
	got, more, err := v.Adjacent(f.t.Context(), node, direction, limit)
	if err != nil {
		f.t.Fatalf("adjacent %+v %s: %v", node, direction, err)
	}
	if len(got) == 0 && len(want) == 0 && more == wantMore {
		return
	}
	if !reflect.DeepEqual(got, want) || more != wantMore {
		f.t.Fatalf("adjacent %+v %s limit %d = %+v more=%v, want %+v more=%v", node, direction, limit, got, more, want, wantMore)
	}
}

func (f fixture) visible(v graph.Snapshot, ref graph.Ref, want bool) {
	f.t.Helper()
	got, err := v.Visible(f.t.Context(), ref)
	if err != nil || got != want {
		f.t.Fatalf("visible %+v = %v %v, want %v", ref, got, err, want)
	}
}

func forwardReverseBoth(f fixture) {
	e1 := f.link("links", f.a(0), f.b(0), "k")
	e2 := f.link("links", f.a(1), f.b(0), "j")
	v := f.view()
	f.adjacent(v, f.a(0), "out", 10, []graph.Edge{e1}, false)
	f.adjacent(v, f.a(0), "in", 10, nil, false)
	f.adjacent(v, f.a(0), "both", 10, []graph.Edge{e1}, false)
	f.adjacent(v, f.b(0), "in", 10, []graph.Edge{e1, e2}, false)
	f.adjacent(v, f.b(0), "out", 10, nil, false)
	f.adjacent(v, f.b(0), "both", 10, []graph.Edge{e1, e2}, false)
	f.adjacent(v, f.b(1), "both", 10, nil, false)
}

func orderAndLimit(f fixture) {
	o1 := f.link("loops", f.a(0), f.a(1), "k")
	l1 := f.link("links", f.a(0), f.b(0), "k")
	l2 := f.link("links", f.a(0), f.b(1), "k")
	v := f.view()
	f.adjacent(v, f.a(0), "out", 10, []graph.Edge{l1, l2, o1}, false)
	f.adjacent(v, f.a(0), "out", 3, []graph.Edge{l1, l2, o1}, false)
	f.adjacent(v, f.a(0), "out", 2, []graph.Edge{l1, l2}, true)
	f.adjacent(v, f.a(0), "out", 1, []graph.Edge{l1}, true)
	f.adjacent(v, f.a(0), "out", 0, nil, true)
	f.adjacent(v, f.a(1), "in", 10, []graph.Edge{o1}, false)
	f.adjacent(v, f.a(1), "out", 0, nil, false)
}

func selfLoopOnce(f fixture) {
	e := f.link("loops", f.a(0), f.a(0), "self")
	v := f.view()
	f.adjacent(v, f.a(0), "both", 10, []graph.Edge{e}, false)
	f.adjacent(v, f.a(0), "out", 10, []graph.Edge{e}, false)
	f.adjacent(v, f.a(0), "in", 10, []graph.Edge{e}, false)
}

func snapshotIsolation(f fixture) {
	e1 := f.link("links", f.a(0), f.b(0), "k")
	old := f.view()
	e2 := f.link("links", f.a(0), f.b(1), "k")
	f.adjacent(old, f.a(0), "out", 10, []graph.Edge{e1}, false)
	f.visible(old, graph.Ref{Table: "links", ID: e2.Ref.ID}, false)
	fresh := f.view()
	f.adjacent(fresh, f.a(0), "out", 10, []graph.Edge{e1, e2}, false)
	f.visible(fresh, e2.Ref, true)
}

func updateMovesEdge(f fixture) {
	e := f.link("links", f.a(0), f.b(0), "k")
	before := f.view()
	f.Update(f.t, "links", e.Ref.ID, map[string]any{"dst": f.beta[1], "kind": "moved"})
	f.adjacent(before, f.b(0), "in", 10, []graph.Edge{e}, false)
	after := f.view()
	moved := graph.Edge{Ref: e.Ref, Source: f.a(0), Target: f.b(1), Type: "moved"}
	f.adjacent(after, f.b(0), "in", 10, nil, false)
	f.adjacent(after, f.b(1), "in", 10, []graph.Edge{moved}, false)
	f.adjacent(after, f.a(0), "out", 10, []graph.Edge{moved}, false)
}

func deleteRemoves(f fixture) {
	e := f.link("links", f.a(0), f.b(0), "k")
	f.Delete(f.t, "links", e.Ref.ID)
	f.Delete(f.t, "beta", f.beta[0])
	v := f.view()
	f.adjacent(v, f.a(0), "out", 10, nil, false)
	f.visible(v, e.Ref, false)
	f.visible(v, f.b(0), false)
	f.visible(v, f.b(1), true)
}

func visibleRows(f fixture) {
	v := f.view()
	f.visible(v, f.a(0), true)
	f.visible(v, graph.Ref{Table: "alpha", ID: f.alpha[1] + 1000}, false)
	f.visible(v, graph.Ref{Table: "missing", ID: f.alpha[0]}, false)
	hidden := f.view("alpha")
	f.visible(hidden, f.a(0), false)
	f.visible(hidden, f.b(0), true)
}

func hiddenRelationTable(f fixture) {
	f.link("links", f.a(0), f.b(0), "k")
	o := f.link("loops", f.a(0), f.a(1), "k")
	v := f.view("links")
	f.adjacent(v, f.a(0), "out", 10, []graph.Edge{o}, false)
	f.adjacent(v, f.b(0), "in", 10, nil, false)
}

func droppedRelationTable(f fixture) {
	f.link("links", f.a(0), f.b(0), "k")
	o := f.link("loops", f.a(0), f.a(1), "k")
	f.Drop(f.t, "links")
	v := f.view()
	f.adjacent(v, f.a(0), "out", 10, []graph.Edge{o}, false)
	f.adjacent(v, f.b(0), "in", 10, nil, false)
	f.visible(v, graph.Ref{Table: "links", ID: 1}, false)
}

func recreatedRelationTable(f fixture) {
	old := f.link("links", f.a(0), f.b(0), "old")
	before := f.view()
	f.Drop(f.t, "links")
	f.CreateRelation(f.t, "links", "alpha", "beta")
	f.adjacent(before, f.a(0), "out", 10, []graph.Edge{old}, false)
	v := f.view()
	f.adjacent(v, f.a(0), "out", 10, nil, false)
	f.adjacent(v, f.b(0), "in", 10, nil, false)
	fresh := f.link("links", f.a(1), f.b(1), "new")
	v = f.view()
	f.adjacent(v, f.a(0), "out", 10, nil, false)
	f.adjacent(v, f.b(0), "in", 10, nil, false)
	f.adjacent(v, f.a(1), "out", 10, []graph.Edge{fresh}, false)
}

func refusesBadInput(f fixture) {
	v := f.view()
	for _, c := range []struct {
		direction string
		limit     int
	}{{"sideways", 10}, {"", 10}, {"out", -1}, {"out", 1001}} {
		if _, _, err := v.Adjacent(f.t.Context(), f.a(0), c.direction, c.limit); err == nil {
			f.t.Fatalf("adjacent accepted direction %q limit %d", c.direction, c.limit)
		}
	}
}

func closeIdempotent(f fixture) {
	v, err := f.Index().Snapshot(f.t.Context(), f.Namespace())
	if err != nil {
		f.t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		f.t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.adjacent(f.view(), f.a(0), "out", 10, nil, false)
}

func canceledSnapshot(f fixture) {
	ctx, cancel := context.WithCancel(f.t.Context())
	cancel()
	if v, err := f.Index().Snapshot(ctx, f.Namespace()); !errors.Is(err, context.Canceled) {
		if v != nil {
			v.Close()
		}
		f.t.Fatalf("canceled snapshot: %v", err)
	}
}
