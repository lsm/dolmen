package dolmen

import (
	"context"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

type TraversalOptions struct {
	Direction                            string
	RelationTypes                        []string
	MaxHops, MaxNodes, MaxEdges, MaxWork int
	MaxElapsed                           time.Duration
}

type GraphResult = NeighborsResult

func (s *Store) FindPath(ctx context.Context, namespace string, source, target NodeRef, opts TraversalOptions) (GraphResult, error) {
	return s.traverse(ctx, "find_path", namespace, []NodeRef{source}, &target, opts)
}
func (s *Store) Subgraph(ctx context.Context, namespace string, seeds []NodeRef, opts TraversalOptions) (GraphResult, error) {
	return s.traverse(ctx, "subgraph", namespace, seeds, nil, opts)
}
func (s *Store) traverse(ctx context.Context, name, namespace string, seeds []NodeRef, target *NodeRef, opts TraversalOptions) (out GraphResult, err error) {
	ctx, span := s.startOp(ctx, name, namespace, "")
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
		return out, derr.New(derr.InvalidRequest, "graph traversal requires the SQLite graph adapter")
	}
	if opts.Direction == "" {
		opts.Direction = "both"
	}
	if opts.MaxHops == 0 {
		opts.MaxHops = 4
	}
	if opts.MaxNodes == 0 {
		opts.MaxNodes = 1000
	}
	if opts.MaxEdges == 0 {
		opts.MaxEdges = 1000
	}
	if opts.MaxWork == 0 {
		opts.MaxWork = 1000
	}
	if opts.MaxElapsed == 0 {
		opts.MaxElapsed = time.Second
	}
	q := graph.TraversalQuery{Seeds: make([]graph.Ref, len(seeds)), Direction: opts.Direction, RelationTypes: opts.RelationTypes, MaxHops: opts.MaxHops, MaxWork: opts.MaxWork, Limits: graph.Limits{Nodes: opts.MaxNodes, Edges: opts.MaxEdges, Elapsed: opts.MaxElapsed}}
	for i, n := range seeds {
		q.Seeds[i] = graph.Ref{Table: ops.NormalizeTable(n.Table), ID: n.ID}
	}
	if target != nil {
		q.Target = &graph.Ref{Table: ops.NormalizeTable(target.Table), ID: target.ID}
	}
	res, err := graph.Traverse(ctx, index.GraphIndex(nil), ops.NormalizeNamespace(namespace), q)
	if err != nil {
		return out, facadeErr(ctx, err)
	}
	out = GraphResult{Nodes: make([]NodeRef, 0, len(res.Nodes)), Edges: make([]NeighborEdge, 0, len(res.Edges)), Truncated: res.Truncated, StopReason: res.StopReason}
	for _, n := range res.Nodes {
		out.Nodes = append(out.Nodes, NodeRef(n))
	}
	for _, e := range res.Edges {
		out.Edges = append(out.Edges, NeighborEdge{Ref: NodeRef(e.Ref), Source: NodeRef(e.Source), Target: NodeRef(e.Target), Type: e.Type})
	}
	return out, nil
}
