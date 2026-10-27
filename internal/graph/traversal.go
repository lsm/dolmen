package graph

import (
	"context"
	"slices"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

type TraversalQuery struct {
	Seeds            []Ref
	Target           *Ref
	Direction        string
	RelationTypes    []string
	Limits           Limits
	MaxHops, MaxWork int
}

type traversalView struct {
	Snapshot
	examined int
}

func (v *traversalView) Close() error { return nil }
func (v *traversalView) Adjacent(ctx context.Context, ref Ref, direction string, limit int) ([]Edge, bool, error) {
	edges, more, err := v.Snapshot.Adjacent(ctx, ref, direction, limit)
	v.examined += len(edges)
	return edges, more, err
}

type traversalIndex struct{ view *traversalView }

func (i traversalIndex) Snapshot(context.Context, string) (Snapshot, error) { return i.view, nil }

func Traverse(ctx context.Context, index Index, namespace string, q TraversalQuery) (Result, error) {
	result := Result{Nodes: []Ref{}, Edges: []Edge{}, StopReason: "complete"}
	valid := func(r Ref) bool { return r.ID > 0 && schema.ValidateTableName(r.Table) == nil }
	if len(q.Seeds) == 0 || len(q.Seeds) > 1000 || (q.Target != nil && (len(q.Seeds) != 1 || !valid(*q.Target))) {
		return result, derr.New(derr.InvalidRequest, "traversal requires 1 to 1000 valid seeds, or one source and a valid target")
	}
	for _, seed := range q.Seeds {
		if !valid(seed) {
			return result, derr.New(derr.InvalidRequest, "traversal references require a valid table and positive int64 id")
		}
	}
	if q.MaxHops < 1 || q.MaxHops > 32 || q.MaxWork < 1 || q.MaxWork > 1000 || q.Limits.Nodes < 1 || q.Limits.Nodes > 1000 || q.Limits.Edges < 1 || q.Limits.Edges > 1000 || q.Limits.Elapsed <= 0 || q.Limits.Elapsed > 30*time.Second || len(q.RelationTypes) > 1000 || (q.Direction != "out" && q.Direction != "in" && q.Direction != "both") {
		return result, derr.New(derr.InvalidRequest, "traversal requires max_hops 1 to 32, node/edge/work limits 1 to 1000, elapsed at most 30 seconds, at most 1000 relation types and direction out, in or both")
	}
	work, cancel := context.WithTimeout(ctx, q.Limits.Elapsed)
	defer cancel()
	finish := func(reason string) (Result, error) {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if work.Err() != nil {
			reason = "elapsed"
		}
		result.Truncated, result.StopReason = reason != "complete", reason
		if q.Target != nil {
			result.Nodes, result.Edges = []Ref{}, []Edge{}
		} else {
			slices.SortFunc(result.Nodes, compareRef)
			slices.SortFunc(result.Edges, func(a, b Edge) int { return compareRef(a.Ref, b.Ref) })
		}
		return result, nil
	}
	fail := func(err error) (Result, error) {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if work.Err() != nil {
			return finish("elapsed")
		}
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	view, err := index.Snapshot(work, namespace)
	if err != nil {
		return fail(err)
	}
	defer view.Close()
	borrowed := &traversalView{Snapshot: view}
	if q.Target != nil {
		visible, err := view.Visible(work, *q.Target)
		if err != nil {
			return fail(err)
		}
		if !visible {
			return finish("complete")
		}
	}
	queue := slices.Clone(q.Seeds)
	slices.SortFunc(queue, compareRef)
	queue = slices.Compact(queue)
	depth := map[Ref]int{}
	parent := map[Ref]Ref{}
	via := map[Ref]Edge{}
	edges := map[Ref]bool{}
	found := func() (Result, error) {
		result.Nodes, result.Edges = []Ref{*q.Target}, []Edge{}
		for node := *q.Target; node != q.Seeds[0]; node = parent[node] {
			result.Edges = append(result.Edges, via[node])
			result.Nodes = append(result.Nodes, parent[node])
		}
		slices.Reverse(result.Nodes)
		slices.Reverse(result.Edges)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if work.Err() != nil {
			return finish("elapsed")
		}
		return result, nil
	}
	visibleSeeds := queue[:0]
	for _, seed := range queue {
		visible, err := view.Visible(work, seed)
		if err != nil {
			return fail(err)
		}
		if !visible {
			continue
		}
		if len(depth) == q.Limits.Nodes {
			return finish("nodes")
		}
		depth[seed] = 0
		result.Nodes = append(result.Nodes, seed)
		visibleSeeds = append(visibleSeeds, seed)
		if q.Target != nil && seed == *q.Target {
			return found()
		}
	}
	queue = visibleSeeds
	for head := 0; head < len(queue); head++ {
		if work.Err() != nil {
			return finish("elapsed")
		}
		node := queue[head]
		if depth[node] == q.MaxHops {
			return finish("hops")
		}
		if borrowed.examined >= q.MaxWork {
			return finish("work")
		}
		next, err := Neighbors(work, traversalIndex{borrowed}, namespace, Query{Node: node, Direction: q.Direction, RelationTypes: q.RelationTypes, Limits: Limits{Nodes: 1000, Edges: q.MaxWork - borrowed.examined, Elapsed: q.Limits.Elapsed}})
		if err != nil {
			return fail(err)
		}
		for _, edge := range next.Edges {
			if edges[edge.Ref] {
				continue
			}
			if len(edges) == q.Limits.Edges {
				return finish("edges")
			}
			other := edge.Target
			if other == node {
				other = edge.Source
			}
			_, seen := depth[other]
			if !seen && len(depth) == q.Limits.Nodes {
				return finish("nodes")
			}
			if !seen {
				depth[other] = depth[node] + 1
				parent[other] = node
				via[other] = edge
				queue = append(queue, other)
				result.Nodes = append(result.Nodes, other)
			}
			edges[edge.Ref] = true
			result.Edges = append(result.Edges, edge)
			if q.Target != nil && other == *q.Target {
				return found()
			}
		}
		if next.Truncated {
			reason := next.StopReason
			if reason == "edges" {
				reason = "work"
			}
			return finish(reason)
		}
	}
	return finish("complete")
}
