package dolmen

import (
	"context"
	"math"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

type GraphSeed struct {
	Ref   NodeRef
	Score float64
}
type GraphSearchResult struct {
	GraphResult
	Seeds          []GraphSeed
	SkippedVectors int
}

func (s *Store) SearchGraph(ctx context.Context, namespace string, tables []string, query VectorQuery, topK int, search SearchOptions, expansion TraversalOptions) (out GraphSearchResult, err error) {
	ctx, span := s.startOp(ctx, "search_graph", namespace, "")
	defer func() { endOp(span, err) }()
	if err = s.begin(); err != nil {
		return out, err
	}
	defer s.done()
	eng, ok := s.eng.(interface {
		SearchGraph(context.Context, string, []string, store.VectorQuery, int, graph.TraversalQuery, store.GraphAccess) (store.GraphSearchResult, error)
	})
	if !ok {
		return out, derr.New(derr.InvalidRequest, "search_graph requires the SQLite graph adapter")
	}
	if err = validateSearchOptions(search); err != nil {
		return out, err
	}
	if query.MinScore != nil && (math.IsNaN(*query.MinScore) || math.IsInf(*query.MinScore, 0)) {
		return out, derr.New(derr.InvalidRequest, "MinScore must be finite")
	}
	if len(tables) == 0 {
		return out, derr.New(derr.InvalidRequest, "search_graph requires seed tables")
	}
	tables = append([]string(nil), tables...)
	for i := range tables {
		tables[i] = ops.NormalizeTable(tables[i])
	}
	vec := make([]float64, len(query.Vec))
	for i, x := range query.Vec {
		vec[i] = float64(x)
	}
	emb := ops.EmbeddingProvider(disabledEmbedding{})
	if s.emb != nil {
		emb = s.tracing.Embedder(s.emb)
	}
	vq, err := ops.PrepareVectorQuery(ctx, s.eng, ops.NormalizeNamespace(namespace), tables[0], ops.VectorQuery{Text: query.Text, Vec: vec, Column: query.Column, MinScore: query.MinScore, Filter: search.Filter, Args: append([]any(nil), search.Args...)}, emb, noProviderHelp)
	if err != nil {
		return out, facadeErr(ctx, err)
	}
	if topK == 0 {
		topK = 10
	}
	expansion = expansion.defaults()
	res, err := eng.SearchGraph(ctx, ops.NormalizeNamespace(namespace), tables, vq, topK, graph.TraversalQuery{Direction: expansion.Direction, RelationTypes: expansion.RelationTypes, MaxHops: expansion.MaxHops, MaxWork: expansion.MaxWork, Limits: graph.Limits{Nodes: expansion.MaxNodes, Edges: expansion.MaxEdges, Elapsed: expansion.MaxElapsed}}, nil)
	if err != nil {
		return out, facadeErr(ctx, err)
	}
	out = GraphSearchResult{GraphResult: graphResult(res.Result), Seeds: make([]GraphSeed, 0, len(res.Seeds)), SkippedVectors: res.SkippedVectors}
	for _, seed := range res.Seeds {
		out.Seeds = append(out.Seeds, GraphSeed{Ref: NodeRef(seed.Ref), Score: seed.Score})
	}
	return out, nil
}
