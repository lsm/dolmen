package store

import (
	"cmp"
	"context"
	"math"
	"slices"
	"strings"

	"github.com/lsm/dolmen/internal/graph"
)

type GraphSeed struct {
	Ref   graph.Ref `json:"ref"`
	Score float64   `json:"score"`
}
type GraphSearchResult struct {
	graph.Result
	Seeds          []GraphSeed `json:"seeds"`
	SkippedVectors int         `json:"skipped_vectors"`
}
type searchGraphView struct{ graph.Snapshot }

func (v searchGraphView) Close() error { return nil }

type searchGraphIndex struct{ view graph.Snapshot }

func (v searchGraphIndex) Snapshot(context.Context, string) (graph.Snapshot, error) {
	return searchGraphView{v.view}, nil
}

func (s *Store) SearchGraph(ctx context.Context, ns string, tables []string, q VectorQuery, topK int, expansion graph.TraversalQuery, access GraphAccess) (out GraphSearchResult, err error) {
	out = GraphSearchResult{Result: graph.Result{Nodes: []graph.Ref{}, Edges: []graph.Edge{}, StopReason: "complete"}, Seeds: []GraphSeed{}}
	if len(tables) == 0 || len(tables) > 100 || topK < 1 || topK > 200 || len(slices.Compact(slices.Sorted(slices.Values(tables)))) != len(tables) {
		return out, invalidf("search_graph requires 1 to 100 unique tables and top_k 1 to 200")
	}
	if err := graph.ValidateTraversalLimits(expansion); err != nil {
		return out, err
	}
	work, cancel := context.WithTimeout(ctx, expansion.Limits.Elapsed)
	defer cancel()
	fail := func(e error) (GraphSearchResult, error) {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if work.Err() != nil {
			out.Truncated = true
			out.StopReason = "elapsed"
			return out, nil
		}
		return out, e
	}
	q.Filter = strings.TrimSpace(q.Filter)
	for i, arg := range q.Args {
		q.Args[i] = normalizeArg(arg)
	}
	view, e := s.GraphIndex(access).Snapshot(work, ns)
	if e != nil {
		return fail(e)
	}
	defer view.Close()
	v := view.(*sqliteGraphSnapshot)
	for _, table := range tables {
		sc := v.schemas[table]
		if sc == nil {
			return out, TableNotFound(ns, table)
		}
		if strings.Contains(q.Filter, ";") || len(q.Args) > 100 {
			return out, invalidf("use a single property filter with at most 100 args")
		}
		column, dim, e := resolveVectorColumn(sc, table, q.Column, q.EmbedModel != "", q.EmbedModel)
		if e != nil {
			return out, e
		}
		if (dim > 0 && len(q.Vec) != dim) || !allFinite(q.Vec) {
			return out, invalidf("query vector must be finite and match column dimension")
		}
		threshold := math.Inf(-1)
		if q.MinScore != nil {
			threshold = *q.MinScore
		}
		hits, skipped, _, e := scanVectors(work, v.tx, table, column, q.Filter, q.Args, v.scopes[table], q.Vec, threshold)
		if e != nil {
			out.Seeds = []GraphSeed{}
			return fail(e)
		}
		out.SkippedVectors += skipped
		for _, hit := range topHits(hits, topK) {
			out.Seeds = append(out.Seeds, GraphSeed{graph.Ref{Table: table, ID: hit.id}, hit.score})
		}
	}
	if work.Err() != nil {
		out.Seeds = []GraphSeed{}
		return fail(work.Err())
	}
	slices.SortFunc(out.Seeds, func(a, b GraphSeed) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), strings.Compare(a.Ref.Table, b.Ref.Table), cmp.Compare(a.Ref.ID, b.Ref.ID))
	})
	out.Seeds = slices.CompactFunc(out.Seeds, func(a, b GraphSeed) bool { return a.Ref == b.Ref })
	if len(out.Seeds) > topK {
		out.Seeds = out.Seeds[:topK]
	}
	if len(out.Seeds) == 0 {
		return out, nil
	}
	expansion.Seeds = nil
	for _, seed := range out.Seeds {
		expansion.Seeds = append(expansion.Seeds, seed.Ref)
	}
	out.Result, e = graph.Traverse(work, searchGraphIndex{view}, ns, expansion)
	if e != nil {
		return fail(e)
	}
	return out, nil
}
