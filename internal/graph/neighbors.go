package graph

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

type Ref struct {
	Table string `json:"table"`
	ID    int64  `json:"id"`
}

type Edge struct {
	Ref    Ref    `json:"ref"`
	Source Ref    `json:"source"`
	Target Ref    `json:"target"`
	Type   string `json:"type"`
}

type Index interface {
	Snapshot(context.Context, string) (Snapshot, error)
}

type Snapshot interface {
	Visible(context.Context, Ref) (bool, error)
	Adjacent(context.Context, Ref, string, int) ([]Edge, bool, error)
	Close() error
}

type Limits struct {
	Nodes, Edges int
	Elapsed      time.Duration
}

type Query struct {
	Node          Ref
	Direction     string
	RelationTypes []string
	Limits        Limits
}

type Result struct {
	Nodes      []Ref  `json:"nodes"`
	Edges      []Edge `json:"edges"`
	Truncated  bool   `json:"truncated"`
	StopReason string `json:"stop_reason"`
}

func compareRef(a, b Ref) int {
	if table := strings.Compare(a.Table, b.Table); table != 0 {
		return table
	}
	if a.ID < b.ID {
		return -1
	}
	if a.ID > b.ID {
		return 1
	}
	return 0
}

func Neighbors(ctx context.Context, index Index, namespace string, q Query) (Result, error) {
	result := Result{Nodes: []Ref{}, Edges: []Edge{}, StopReason: "complete"}
	if q.Node.ID <= 0 || schema.ValidateTableName(q.Node.Table) != nil || (q.Direction != "out" && q.Direction != "in" && q.Direction != "both") {
		return result, derr.New(derr.InvalidRequest, "neighbors requires a node with a valid table and positive int64 id, and direction out, in or both")
	}
	if q.Limits.Nodes < 1 || q.Limits.Nodes > 1000 || q.Limits.Edges < 1 || q.Limits.Edges > 1000 || q.Limits.Elapsed <= 0 || q.Limits.Elapsed > 30*time.Second {
		return result, derr.New(derr.InvalidRequest, "neighbors requires node/edge limits from 1 to 1000 and a positive elapsed limit at most 30 seconds")
	}
	if len(q.RelationTypes) > 1000 {
		return result, derr.New(derr.InvalidRequest, "neighbors accepts at most 1000 relation types; reduce the filter")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	work, cancel := context.WithTimeout(ctx, q.Limits.Elapsed)
	defer cancel()
	finish := func(reason string) (Result, error) {
		slices.SortFunc(result.Nodes, compareRef)
		slices.SortFunc(result.Edges, func(a, b Edge) int { return compareRef(a.Ref, b.Ref) })
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if work.Err() != nil {
			reason = "elapsed"
		}
		result.Truncated, result.StopReason = reason != "complete", reason
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
	view, err := index.Snapshot(work, namespace)
	if err != nil {
		return fail(err)
	}
	defer view.Close()
	visible, err := view.Visible(work, q.Node)
	if err != nil {
		return fail(err)
	}
	if !visible {
		return finish("complete")
	}
	seen := map[Ref]bool{q.Node: true}
	result.Nodes = append(result.Nodes, q.Node)
	edges, more, err := view.Adjacent(work, q.Node, q.Direction, q.Limits.Edges)
	if err != nil {
		return fail(err)
	}
	edges = slices.Clone(edges)
	slices.SortFunc(edges, func(a, b Edge) int { return compareRef(a.Ref, b.Ref) })
	for i, edge := range edges {
		if work.Err() != nil {
			return fail(work.Err())
		}
		if i == q.Limits.Edges {
			return finish("edges")
		}
		if (q.Direction == "out" && edge.Source != q.Node) || (q.Direction == "in" && edge.Target != q.Node) || (edge.Source != q.Node && edge.Target != q.Node) {
			continue
		}
		if len(q.RelationTypes) != 0 && !slices.Contains(q.RelationTypes, edge.Type) {
			continue
		}
		visible = true
		for _, ref := range []Ref{edge.Ref, edge.Source, edge.Target} {
			ok, err := view.Visible(work, ref)
			if err != nil {
				return fail(err)
			}
			if !ok {
				visible = false
				break
			}
		}
		if !visible {
			continue
		}
		needed := 0
		if !seen[edge.Source] {
			needed++
		}
		if edge.Target != edge.Source && !seen[edge.Target] {
			needed++
		}
		if len(seen)+needed > q.Limits.Nodes {
			return finish("nodes")
		}
		for _, ref := range []Ref{edge.Source, edge.Target} {
			if !seen[ref] {
				seen[ref] = true
				result.Nodes = append(result.Nodes, ref)
			}
		}
		result.Edges = append(result.Edges, edge)
	}
	if more {
		return finish("edges")
	}
	return finish("complete")
}
