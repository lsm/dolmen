package main

import (
	"context"
	"fmt"
	"github.com/lsm/dolmen"
	"os"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

type fixedProvider struct{}

func (fixedProvider) Identity() string { return "verify|fixed|2" }
func (fixedProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}
func (fixedProvider) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func main() {
	dir, err := os.MkdirTemp("", "dolmen-search-")
	must(err)
	defer os.RemoveAll(dir)
	s, err := dolmen.Open(dir, dolmen.WithEmbedding(fixedProvider{}))
	must(err)
	defer s.Close()
	ctx := context.Background()
	must(s.CreateNamespace(ctx, "demo"))
	_, err = s.CreateTable(ctx, "demo", "nodes", []dolmen.Field{{Name: "v", Type: dolmen.Vector, Dim: 2}, {Name: "label", Type: dolmen.String}})
	must(err)
	_, err = s.Insert(ctx, "demo", "nodes", []map[string]any{{"v": []float32{1, 0}, "label": "keep"}, {"v": []float32{0, 1}, "label": "drop"}}, dolmen.InsertOptions{})
	must(err)
	out, err := s.SearchGraph(ctx, "demo", []string{"nodes"}, dolmen.VectorQuery{Vec: []float32{1, 0}}, 1, dolmen.SearchOptions{Filter: "label = ?", Args: []any{"keep"}}, dolmen.TraversalOptions{})
	must(err)
	if len(out.Seeds) != 1 || out.Seeds[0].Score != 1 || len(out.Nodes) != 1 || out.Truncated {
		panic(fmt.Sprintf("unexpected result: %+v", out))
	}
	fmt.Printf("PASS Go SearchGraph: seed id=%d score=%g, %d node, complete\n", out.Seeds[0].Ref.ID, out.Seeds[0].Score, len(out.Nodes))
	_, err = s.CreateTable(ctx, "demo", "empty_docs", []dolmen.Field{{Name: "body", Type: dolmen.Text, Vectorize: true}})
	must(err)
	baseline, err := s.SearchVector(ctx, "demo", "empty_docs", dolmen.VectorQuery{Vec: []float32{1, 0}}, dolmen.SearchOptions{})
	must(err)
	if len(baseline.Rows) != 0 {
		panic("empty semantic baseline unexpectedly has rows")
	}
	out, err = s.SearchGraph(ctx, "demo", []string{"empty_docs"}, dolmen.VectorQuery{Vec: []float32{1, 0}}, 1, dolmen.SearchOptions{}, dolmen.TraversalOptions{})
	must(err)
	if len(out.Seeds) != 0 || len(out.Nodes) != 0 || out.Truncated {
		panic("empty managed space unexpectedly has seeds")
	}
	fmt.Println("PASS Go SearchGraph: empty managed vector table returns no seeds or connections")
	_, err = s.Insert(ctx, "demo", "empty_docs", []map[string]any{{"body": "synthetic"}}, dolmen.InsertOptions{})
	must(err)
	out, err = s.SearchGraph(ctx, "demo", []string{"empty_docs"}, dolmen.VectorQuery{Text: "synthetic"}, 1, dolmen.SearchOptions{}, dolmen.TraversalOptions{})
	must(err)
	if len(out.Seeds) != 1 || out.Seeds[0].Score != 1 || out.Truncated {
		panic("synthetic text seed was not found")
	}
	fmt.Println("PASS Go SearchGraph: synthetic embedding provider returns a text-query seed with score=1")

}
