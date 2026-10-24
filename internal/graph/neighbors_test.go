package graph

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/derr"
)

type testIndex struct {
	view      *testView
	snapshots int
}

func (i *testIndex) Snapshot(ctx context.Context, ns string) (Snapshot, error) {
	i.snapshots++
	return i.view, nil
}

type testView struct {
	edges         []Edge
	hidden        map[Ref]bool
	checked       []Ref
	calls, closed int
	more          bool
	wait          bool
	failure       error
}

func (v *testView) Visible(ctx context.Context, r Ref) (bool, error) {
	v.checked = append(v.checked, r)
	return !v.hidden[r], nil
}
func (v *testView) Adjacent(ctx context.Context, r Ref, direction string, limit int) ([]Edge, bool, error) {
	v.calls++
	if v.wait {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	if v.failure != nil {
		return nil, false, v.failure
	}
	edges := v.edges
	more := v.more
	if len(edges) > limit {
		edges = edges[:limit]
		more = true
	}
	return edges, more, nil
}
func (v *testView) Close() error { v.closed++; return nil }
func query() Query {
	return Query{Node: Ref{"nodes", 1}, Direction: "both", Limits: Limits{Nodes: 10, Edges: 10, Elapsed: time.Second}}
}
func edge(id, from, to int64, typ string) Edge {
	return Edge{Ref: Ref{"links", id}, Source: Ref{"nodes", from}, Target: Ref{"nodes", to}, Type: typ}
}

func TestNeighborsFiltersEveryEdgeAndEndpointBeforeReturning(t *testing.T) {
	q := query()
	v := &testView{edges: []Edge{edge(1, 1, 2, "k"), edge(2, 1, 3, "k"), edge(3, 4, 1, "k"), edge(4, 1, 1, "loop")}, hidden: map[Ref]bool{{"links", 2}: true, {"nodes", 4}: true}}
	got, err := Neighbors(t.Context(), &testIndex{view: v}, "demo", q)
	if err != nil || got.Truncated || got.StopReason != "complete" {
		t.Fatalf("result %+v %v", got, err)
	}
	if !reflect.DeepEqual(got.Nodes, []Ref{{"nodes", 1}, {"nodes", 2}}) || !reflect.DeepEqual(got.Edges, []Edge{edge(1, 1, 2, "k"), edge(4, 1, 1, "loop")}) {
		t.Fatalf("visibility/self loop: %+v", got)
	}
	if v.closed != 1 {
		t.Fatalf("snapshot closed %d times", v.closed)
	}
	for _, r := range []Ref{{"nodes", 1}, {"links", 1}, {"nodes", 2}, {"links", 2}, {"nodes", 4}} {
		found := false
		for _, seen := range v.checked {
			if seen == r {
				found = true
			}
		}
		if !found {
			t.Fatalf("visibility never checked %+v", r)
		}
	}
}

func TestNeighborsDoesNotExpandHiddenSeed(t *testing.T) {
	v := &testView{hidden: map[Ref]bool{{"nodes", 1}: true}, edges: []Edge{edge(1, 1, 2, "k")}}
	got, err := Neighbors(t.Context(), &testIndex{view: v}, "demo", query())
	if err != nil || len(got.Nodes) != 0 || len(got.Edges) != 0 || v.calls != 0 || got.Truncated {
		t.Fatalf("hidden seed expanded: %+v %v", got, err)
	}
}

func TestNeighborsBoundsWholeEdgesAndReportsWhy(t *testing.T) {
	for _, limit := range []string{"nodes", "edges"} {
		t.Run(limit, func(t *testing.T) {
			q := query()
			v := &testView{edges: []Edge{edge(1, 1, 2, "k"), edge(2, 1, 3, "k")}}
			if limit == "nodes" {
				q.Limits.Nodes = 2
			} else {
				q.Limits.Edges = 1
			}
			got, err := Neighbors(t.Context(), &testIndex{view: v}, "demo", q)
			if err != nil || !got.Truncated || got.StopReason != limit || len(got.Edges) != 1 || len(got.Nodes) != 2 {
				t.Fatalf("bounded result %+v %v", got, err)
			}
		})
	}
	v := &testView{wait: true}
	q := query()
	q.Limits.Elapsed = time.Millisecond
	got, err := Neighbors(t.Context(), &testIndex{view: v}, "demo", q)
	if err != nil || !got.Truncated || got.StopReason != "elapsed" || v.closed != 1 {
		t.Fatalf("elapsed result %+v %v", got, err)
	}
}

func TestNeighborsStableReferencesDirectionsAndTypes(t *testing.T) {
	for _, direction := range []string{"out", "in", "both"} {
		t.Run(direction, func(t *testing.T) {
			q := query()
			q.Direction = direction
			q.RelationTypes = []string{"k"}
			v := &testView{edges: []Edge{edge(3, 1, 2, "other"), edge(2, 3, 1, "k"), edge(1, 1, 2, "k")}}
			got, err := Neighbors(t.Context(), &testIndex{view: v}, "demo", q)
			want := []Edge{edge(1, 1, 2, "k"), edge(2, 3, 1, "k")}
			if direction == "out" {
				want = want[:1]
			}
			if direction == "in" {
				want = want[1:]
			}
			if err != nil || !reflect.DeepEqual(got.Edges, want) {
				t.Fatalf("direction/types: %+v %v", got, err)
			}
		})
	}
}

func TestNeighborsSeparatesCancellationAndBackendFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	idx := &testIndex{view: &testView{}}
	if _, err := Neighbors(ctx, idx, "demo", query()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if idx.snapshots != 0 {
		t.Fatal("opened snapshot after cancellation")
	}
	failure := errors.New("synthetic storage fault")
	v := &testView{failure: failure}
	if _, err := Neighbors(t.Context(), &testIndex{view: v}, "demo", query()); !errors.Is(err, failure) {
		t.Fatalf("masked failure: %v", err)
	}
	if v.closed != 1 {
		t.Fatal("failed snapshot not closed")
	}
}

func TestNeighborsRejectsInvalidInputBeforeOpeningSnapshot(t *testing.T) {
	for _, change := range []func(*Query){func(q *Query) { q.Node.ID = 0 }, func(q *Query) { q.Node.Table = "bad-name" }, func(q *Query) { q.Direction = "sideways" }, func(q *Query) { q.Limits.Nodes = 0 }, func(q *Query) { q.Limits.Edges = 1001 }, func(q *Query) { q.Limits.Elapsed = 0 }} {
		q := query()
		change(&q)
		idx := &testIndex{view: &testView{}}
		if _, err := Neighbors(t.Context(), idx, "demo", q); !errors.Is(err, derr.ErrInvalidRequest) {
			t.Fatalf("invalid request: %v", err)
		}
		if idx.snapshots != 0 {
			t.Fatal("opened snapshot for invalid input")
		}
	}
}
