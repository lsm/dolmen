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
func main() {
	dir, err := os.MkdirTemp("", "dolmen-search-")
	must(err)
	defer os.RemoveAll(dir)
	s, err := dolmen.Open(dir)
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
}
