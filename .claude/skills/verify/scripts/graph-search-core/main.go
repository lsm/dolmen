package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func expect(ok bool, message string) {
	if !ok {
		panic(message)
	}
}

func main() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "dolmen-graph-sqlite-")
	must(err)
	defer os.RemoveAll(dir)
	s, err := store.Open(dir)
	must(err)
	defer s.Close()
	must(s.CreateNamespace(ctx, "verify", [16]byte{}))
	_, err = s.CreateTable(ctx, "verify", "nodes", []schema.Field{{Name: "label", Type: schema.String}, {Name: "v", Type: schema.Vector, Dim: 2}}, store.TableOpts{RowAccess: "own", AcceptReferences: true}, [16]byte{})
	must(err)
	fields := []schema.Field{{Name: "src", Type: schema.Number, Required: true}, {Name: "dst", Type: schema.Number, Required: true}, {Name: "kind", Type: schema.String, Required: true}}
	constraints := []schema.Constraint{
		{Name: "src_ref", Kind: schema.ConstraintForeignKey, Field: "src", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
		{Name: "dst_ref", Kind: schema.ConstraintForeignKey, Field: "dst", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
	}
	_, err = s.CreateTable(ctx, "verify", "links", fields, store.TableOpts{RowAccess: "own", Constraints: constraints, Relation: &schema.Relation{Source: "src", Target: "dst", Type: "kind"}}, [16]byte{})
	must(err)
	for _, owner := range []string{"alice", "bob"} {
		ins, err := s.Insert(ctx, "verify", "nodes", []map[string]any{{"label": "one", "v": []float32{1, 0}}, {"label": "two", "v": []float32{0, 1}}}, store.WriteOpts{Owner: owner}, store.Embedder{}, nil, store.Incarnation{})
		must(err)
		_, err = s.Insert(ctx, "verify", "links", []map[string]any{{"src": ins.Ids[0], "dst": ins.Ids[1], "kind": "knows"}}, store.WriteOpts{Owner: owner}, store.Embedder{}, nil, store.Incarnation{})
		must(err)
	}
	access := func(context.Context, *schema.TableSchema) (*store.RowScope, error) {
		return &store.RowScope{Owner: "alice"}, nil
	}

	expansion := graph.TraversalQuery{Direction: "both", MaxHops: 4, MaxWork: 1000, Limits: graph.Limits{Nodes: 1000, Edges: 1000, Elapsed: time.Second}}
	q := store.VectorQuery{Vec: []float32{1, 0}, Filter: "label = ?", Args: []any{"one"}}
	out, err := s.SearchGraph(ctx, "verify", []string{"nodes"}, q, 1, expansion, access)
	must(err)
	expect(len(out.Seeds) == 1 && out.Seeds[0].Ref.ID == 1 && out.Seeds[0].Score == 1 && len(out.Nodes) == 2 && len(out.Edges) == 1 && !out.Truncated, "scoped seeds and expansion")
	fmt.Println("PASS SQLite SearchGraph: own-row seed id=1 score=1, 2 visible nodes, 1 visible edge, complete")
	expansion.Limits.Nodes = 1
	out, err = s.SearchGraph(ctx, "verify", []string{"nodes"}, q, 1, expansion, access)
	must(err)
	expect(out.Truncated && out.StopReason == "nodes" && len(out.Edges) == 0, "node bound")
	fmt.Println("PASS SQLite SearchGraph: max_nodes=1 returns truncated/nodes and no dangling edge")
	expansion.Limits.Nodes = 1000
	expansion.RelationTypes = []string{"absent"}
	out, err = s.SearchGraph(ctx, "verify", []string{"nodes"}, q, 1, expansion, access)
	must(err)
	expect(!out.Truncated && len(out.Nodes) == 1 && len(out.Edges) == 0, "relation filter")
	fmt.Println("PASS SQLite SearchGraph: property/type filters preserve the seed and exclude connections")
	expansion.Limits.Elapsed = time.Millisecond
	out, err = s.SearchGraph(ctx, "verify", []string{"nodes"}, q, 1, expansion, func(ctx context.Context, _ *schema.TableSchema) (*store.RowScope, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	must(err)
	expect(out.Truncated && out.StopReason == "elapsed", "elapsed bound during snapshot setup")
	fmt.Println("PASS SQLite SearchGraph: blocked snapshot setup returns truncated/elapsed")

}
