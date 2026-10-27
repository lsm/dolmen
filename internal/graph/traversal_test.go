package graph

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type walkView struct {
	edges         []Edge
	hidden        map[Ref]bool
	calls, closed int
	wait          bool
	fault         error
}

func (v *walkView) Visible(ctx context.Context, r Ref) (bool, error) { return !v.hidden[r], nil }
func (v *walkView) Adjacent(ctx context.Context, r Ref, direction string, limit int) ([]Edge, bool, error) {
	v.calls++
	if v.wait {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	if v.fault != nil {
		return nil, false, v.fault
	}
	out := []Edge{}
	for _, e := range v.edges {
		if (direction != "in" && e.Source == r) || (direction != "out" && e.Target == r) {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
func (v *walkView) Close() error { v.closed++; return nil }

type walkIndex struct {
	view  *walkView
	opens int
	wait  bool
}

func (i *walkIndex) Snapshot(ctx context.Context, ns string) (Snapshot, error) {
	i.opens++
	if i.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return i.view, nil
}
func traversal() TraversalQuery {
	return TraversalQuery{Seeds: []Ref{{"nodes", 1}}, Direction: "out", Limits: Limits{Nodes: 1000, Edges: 1000, Elapsed: time.Second}, MaxHops: 4, MaxWork: 1000}
}
func chain() *walkView { return &walkView{edges: []Edge{edge(1, 1, 2, "k"), edge(2, 2, 3, "k")}} }

func TestTraversalBoundsBothOperations(t *testing.T) {
	for _, path := range []bool{false, true} {
		for _, reason := range []string{"hops", "nodes", "edges", "work", "elapsed"} {
			t.Run(reason+map[bool]string{true: "Path", false: "Subgraph"}[path], func(t *testing.T) {
				q := traversal()
				if path {
					target := Ref{"nodes", 3}
					q.Target = &target
				}
				v := chain()
				switch reason {
				case "hops":
					q.MaxHops = 1
				case "nodes":
					q.Limits.Nodes = 1
				case "edges":
					q.Limits.Edges = 1
				case "work":
					q.MaxWork = 1
				case "elapsed":
					q.Limits.Elapsed = time.Millisecond
					v.wait = true
				}
				idx := &walkIndex{view: v}
				got, err := Traverse(t.Context(), idx, "ns", q)
				if err != nil || !got.Truncated || got.StopReason != reason {
					t.Fatalf("%+v %v", got, err)
				}
				if len(got.Nodes) > q.Limits.Nodes || len(got.Edges) > q.Limits.Edges {
					t.Fatalf("bound exceeded: %+v", got)
				}
				if path && (len(got.Nodes) != 0 || len(got.Edges) != 0) {
					t.Fatalf("bounded miss masquerades as a path: %+v", got)
				}
				if idx.opens != 1 || v.closed != 1 {
					t.Fatalf("snapshot ownership: %d/%d", idx.opens, v.closed)
				}
			})
		}
	}
}

func TestTraversalShortestStablePathAndDirections(t *testing.T) {
	v := &walkView{edges: []Edge{edge(1, 1, 2, "k"), edge(2, 1, 4, "k"), edge(3, 2, 3, "k"), edge(4, 4, 3, "k"), edge(5, 3, 3, "k"), edge(6, 3, 1, "back")}}
	q := traversal()
	target := Ref{"nodes", 3}
	q.Target = &target
	q.RelationTypes = []string{"k"}
	original := append([]Edge(nil), v.edges...)
	for _, direction := range []string{"out", "in", "both"} {
		q.Direction = direction
		if direction == "in" {
			q.Seeds = []Ref{{"nodes", 3}}
			target = Ref{"nodes", 1}
		} else {
			q.Seeds = []Ref{{"nodes", 1}}
			target = Ref{"nodes", 3}
		}
		got, err := Traverse(t.Context(), &walkIndex{view: v}, "ns", q)
		want := []Ref{{"nodes", 1}, {"nodes", 2}, {"nodes", 3}}
		if direction == "in" {
			want = []Ref{{"nodes", 3}, {"nodes", 2}, {"nodes", 1}}
		}
		if err != nil || got.Truncated || got.StopReason != "complete" || !reflect.DeepEqual(got.Nodes, want) || len(got.Edges) != 2 {
			t.Fatalf("%s: %+v %v", direction, got, err)
		}
	}
	if !reflect.DeepEqual(v.edges, original) {
		t.Fatal("mutated adjacency")
	}
}

func TestTraversalSeedsCyclesAndIdentity(t *testing.T) {
	q := traversal()
	q.Direction = "both"
	q.Seeds = []Ref{{"nodes", 2}, {"nodes", 1}, {"nodes", 2}}
	v := &walkView{edges: []Edge{edge(1, 1, 2, "k"), edge(2, 2, 1, "k"), edge(3, 2, 2, "k"), {Ref: Ref{"links", 4}, Source: Ref{"nodes", 2}, Target: Ref{"other", 1}, Type: "k"}}}
	got, err := Traverse(t.Context(), &walkIndex{view: v}, "ns", q)
	if err != nil || got.Truncated || !reflect.DeepEqual(got.Nodes, []Ref{{"nodes", 1}, {"nodes", 2}, {"other", 1}}) || len(got.Edges) != 4 {
		t.Fatalf("dedup: %+v %v", got, err)
	}
	if !reflect.DeepEqual(q.Seeds, []Ref{{"nodes", 2}, {"nodes", 1}, {"nodes", 2}}) {
		t.Fatal("mutated caller seeds")
	}
	q.Seeds = []Ref{{"nodes", 1}}
	q.Target = &q.Seeds[0]
	q.MaxWork = 1
	q.Limits.Nodes = 1
	q.Limits.Edges = 1
	got, err = Traverse(t.Context(), &walkIndex{view: v}, "ns", q)
	if err != nil || got.Truncated || len(got.Nodes) != 1 || len(got.Edges) != 0 {
		t.Fatalf("identity path: %+v %v", got, err)
	}
}

func TestTraversalVisibilityAndFilteredWork(t *testing.T) {
	for _, hidden := range []Ref{{"nodes", 1}, {"nodes", 2}, {"nodes", 3}, {"links", 1}} {
		q := traversal()
		target := Ref{"nodes", 3}
		q.Target = &target
		v := chain()
		v.hidden = map[Ref]bool{hidden: true}
		got, err := Traverse(t.Context(), &walkIndex{view: v}, "ns", q)
		if err != nil || got.Truncated || len(got.Nodes) != 0 || len(got.Edges) != 0 {
			t.Fatalf("hidden %+v: %+v %v", hidden, got, err)
		}
		q.Target = nil
		got, err = Traverse(t.Context(), &walkIndex{view: v}, "ns", q)
		for _, n := range got.Nodes {
			if n == hidden {
				t.Fatalf("hidden node leaked: %+v", got)
			}
		}
	}
	q := traversal()
	q.MaxWork = 1
	q.RelationTypes = []string{"absent"}
	v := &walkView{edges: []Edge{edge(1, 1, 2, "k"), edge(2, 1, 3, "k")}}
	got, err := Traverse(t.Context(), &walkIndex{view: v}, "ns", q)
	if err != nil || !got.Truncated || got.StopReason != "work" || len(got.Edges) != 0 {
		t.Fatalf("filtered work was free: %+v %v", got, err)
	}
}

func TestTraversalErrorsAndValidation(t *testing.T) {
	q := traversal()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	idx := &walkIndex{view: chain()}
	if _, err := Traverse(ctx, idx, "ns", q); !errors.Is(err, context.Canceled) || idx.opens != 0 {
		t.Fatalf("cancellation: %v/%d", err, idx.opens)
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	v := chain()
	v.wait = true
	if _, err := Traverse(ctx, &walkIndex{view: v}, "ns", q); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent timeout: %v", err)
	}
	failure := errors.New("storage fault")
	v = chain()
	v.fault = failure
	if _, err := Traverse(t.Context(), &walkIndex{view: v}, "ns", q); !errors.Is(err, failure) || v.closed != 1 {
		t.Fatalf("storage fault: %v", err)
	}
	q.Limits.Elapsed = time.Millisecond
	got, err := Traverse(t.Context(), &walkIndex{wait: true}, "ns", q)
	if err != nil || !got.Truncated || got.StopReason != "elapsed" {
		t.Fatalf("snapshot work deadline: %+v %v", got, err)
	}
	for _, change := range []func(*TraversalQuery){func(q *TraversalQuery) { q.Seeds = nil }, func(q *TraversalQuery) { q.Seeds[0].ID = 0 }, func(q *TraversalQuery) { q.MaxHops = 0 }, func(q *TraversalQuery) { q.MaxHops = 33 }, func(q *TraversalQuery) { q.MaxWork = 0 }, func(q *TraversalQuery) { q.MaxWork = 1001 }, func(q *TraversalQuery) { q.Limits.Nodes = 0 }, func(q *TraversalQuery) { q.Limits.Edges = 1001 }, func(q *TraversalQuery) { q.Limits.Elapsed = 0 }, func(q *TraversalQuery) { q.Direction = "sideways" }, func(q *TraversalQuery) { q.Target = &Ref{"nodes", 0} }} {
		q := traversal()
		change(&q)
		idx := &walkIndex{view: chain()}
		if _, err := Traverse(t.Context(), idx, "ns", q); err == nil || idx.opens != 0 {
			t.Fatalf("invalid request reached storage: %v/%d", err, idx.opens)
		}
	}
}
