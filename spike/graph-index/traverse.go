package graphbench

import (
	"context"
	"fmt"
	"math"
	"slices"
)

func Validate(edges []Edge) error {
	for _, e := range edges {
		if e.ID == 0 || e.From == 0 || e.To == 0 || e.ID > math.MaxInt64 || e.From > math.MaxInt64 || e.To > math.MaxInt64 || e.Kind == 0 {
			return fmt.Errorf("edges require positive int64 IDs, endpoints and kinds")
		}
	}
	return nil
}

func Traverse(ctx context.Context, v View, q Query) (Result, error) {
	r := Result{Stop: "complete"}
	if q.Hops < 0 || q.Nodes < 1 || q.Edges < 1 || q.Elapsed <= 0 {
		return r, fmt.Errorf("provide nonnegative hops and positive node, edge and elapsed limits")
	}
	work, cancel := context.WithTimeout(ctx, q.Elapsed)
	defer cancel()
	seen := map[uint64]bool{}
	parents := map[uint64]uint64{}
	edgeIDs := map[uint64]bool{}
	type item struct {
		node  uint64
		depth int
	}
	queue := []item{}
	finish := func() (Result, error) {
		for n := range seen {
			r.Nodes = append(r.Nodes, n)
		}
		for id := range edgeIDs {
			r.EdgeIDs = append(r.EdgeIDs, id)
		}
		slices.Sort(r.Nodes)
		slices.Sort(r.EdgeIDs)
		return r, nil
	}
	stop := func(reason string) (Result, error) { r.Truncated = true; r.Stop = reason; return finish() }
	visible := func(n uint64) bool { return q.Visible == nil || q.Visible(n) }
	seeds := slices.Clone(q.Seeds)
	slices.Sort(seeds)
	for _, n := range seeds {
		if n == 0 || seen[n] || !visible(n) {
			continue
		}
		if len(seen) == q.Nodes {
			return stop("nodes")
		}
		seen[n] = true
		queue = append(queue, item{n, 0})
	}
	for head := 0; head < len(queue); head++ {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if work.Err() != nil {
			return stop("elapsed")
		}
		current := queue[head]
		if current.node == q.Target {
			for n := q.Target; n != 0; n = parents[n] {
				r.Path = append(r.Path, n)
			}
			slices.Reverse(r.Path)
			r.Stop = "found"
			r.Truncated = false
			return finish()
		}
		if current.depth == q.Hops {
			if q.Target != 0 {
				r.Truncated = true
				r.Stop = "hops"
			}
			continue
		}
		edges, more, err := v.Adjacent(work, current.node, q.Reverse, q.Edges-r.Scanned)
		if err != nil {
			if ctx.Err() != nil {
				return r, ctx.Err()
			}
			if work.Err() != nil {
				return stop("elapsed")
			}
			return r, err
		}
		for _, e := range edges {
			if err := ctx.Err(); err != nil {
				return r, err
			}
			if work.Err() != nil {
				return stop("elapsed")
			}
			r.Scanned++
			n := e.To
			if q.Reverse {
				n = e.From
			}
			if (q.Kind != 0 && e.Kind != q.Kind) || !visible(e.From) || !visible(e.To) || (q.EdgeVisible != nil && !q.EdgeVisible(e)) {
				continue
			}
			if !seen[n] {
				if len(seen) == q.Nodes {
					return stop("nodes")
				}
				seen[n] = true
				parents[n] = current.node
				queue = append(queue, item{n, current.depth + 1})
			}
			edgeIDs[e.ID] = true
			if n == q.Target {
				for p := n; p != 0; p = parents[p] {
					r.Path = append(r.Path, p)
				}
				slices.Reverse(r.Path)
				r.Stop = "found"
				r.Truncated = false
				return finish()
			}
		}
		if more {
			return stop("edges")
		}
	}
	return finish()
}
