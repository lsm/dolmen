package dolmen

import (
	"context"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

type NodeRef struct {
	Table string
	ID    int64
}

type NeighborEdge struct {
	Ref, Source, Target NodeRef
	Type                string
}

type NeighborsOptions struct {
	Direction          string
	RelationTypes      []string
	MaxNodes, MaxEdges int
	MaxElapsed         time.Duration
}

type NeighborsResult struct {
	Nodes      []NodeRef
	Edges      []NeighborEdge
	Truncated  bool
	StopReason string
}

func (s *Store) Neighbors(ctx context.Context, namespace string, node NodeRef, opts NeighborsOptions) (out NeighborsResult, err error) {
	ctx, span := s.startOp(ctx, "neighbors", namespace, node.Table)
	defer func() { endOp(span, err) }()
	if err = s.begin(); err != nil {
		return out, err
	}
	defer s.done()
	if err = ctx.Err(); err != nil {
		return out, facadeErr(ctx, err)
	}
	index, ok := s.eng.(interface {
		GraphIndex(store.GraphAccess) graph.Index
	})
	if !ok {
		return out, derr.New(derr.InvalidRequest, "neighbors requires the SQLite graph adapter")
	}
	if opts.Direction == "" {
		opts.Direction = "both"
	}
	if opts.MaxNodes == 0 {
		opts.MaxNodes = 1000
	}
	if opts.MaxEdges == 0 {
		opts.MaxEdges = 1000
	}
	if opts.MaxElapsed == 0 {
		opts.MaxElapsed = time.Second
	}
	res, err := graph.Neighbors(ctx, index.GraphIndex(nil), ops.NormalizeNamespace(namespace), graph.Query{Node: graph.Ref{Table: ops.NormalizeTable(node.Table), ID: node.ID}, Direction: opts.Direction, RelationTypes: opts.RelationTypes, Limits: graph.Limits{Nodes: opts.MaxNodes, Edges: opts.MaxEdges, Elapsed: opts.MaxElapsed}})
	if err != nil {
		return out, facadeErr(ctx, err)
	}
	out = NeighborsResult{Nodes: make([]NodeRef, 0, len(res.Nodes)), Edges: make([]NeighborEdge, 0, len(res.Edges)), Truncated: res.Truncated, StopReason: res.StopReason}
	for _, n := range res.Nodes {
		out.Nodes = append(out.Nodes, NodeRef(n))
	}
	for _, e := range res.Edges {
		out.Edges = append(out.Edges, NeighborEdge{Ref: NodeRef(e.Ref), Source: NodeRef(e.Source), Target: NodeRef(e.Target), Type: e.Type})
	}
	return out, nil
}
