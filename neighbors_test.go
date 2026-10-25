package dolmen_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/lsm/dolmen"
)

func declaredNeighborStore(t *testing.T) *dolmen.Store {
	t.Helper()
	s, err := dolmen.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := t.Context()
	if _, err := s.CreateTableWithOptions(ctx, "demo", "nodes", []dolmen.Field{{Name: "label", Type: dolmen.String}}, dolmen.CreateTableOptions{AcceptReferences: true}); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateTableWithOptions(ctx, "demo", "links", []dolmen.Field{{Name: "src", Type: dolmen.Number, Required: true}, {Name: "dst", Type: dolmen.Number, Required: true}, {Name: "kind", Type: dolmen.String, Required: true}}, dolmen.CreateTableOptions{
		Constraints: []dolmen.Constraint{dolmen.ForeignKeyConstraint{Name: "src_ref", Field: "src", ReferencesTable: "nodes"}, dolmen.ForeignKeyConstraint{Name: "dst_ref", Field: "dst", ReferencesTable: "nodes"}},
		Relation:    &dolmen.Relation{Source: "src", Target: "dst", Type: "kind"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "demo", "nodes", []map[string]any{{"label": "first"}, {"label": "second"}}, dolmen.InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Relate(ctx, "demo", "links", dolmen.Edge{Source: 1, Target: 2, Type: "knows"}, dolmen.InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNeighborsPublicDeclaredRelations(t *testing.T) {
	s := declaredNeighborStore(t)
	result, err := s.Neighbors(t.Context(), " Demo ", dolmen.NodeRef{Table: " Nodes ", ID: 1}, dolmen.NeighborsOptions{Direction: "out"})
	if err != nil || result.Truncated || result.StopReason != "complete" || !reflect.DeepEqual(result.Nodes, []dolmen.NodeRef{{Table: "nodes", ID: 1}, {Table: "nodes", ID: 2}}) {
		t.Fatalf("public neighbors: %+v %v", result, err)
	}
	if len(result.Edges) != 1 || result.Edges[0].Ref != (dolmen.NodeRef{Table: "links", ID: 1}) || result.Edges[0].Source != (dolmen.NodeRef{Table: "nodes", ID: 1}) || result.Edges[0].Target != (dolmen.NodeRef{Table: "nodes", ID: 2}) || result.Edges[0].Type != "knows" {
		t.Fatalf("public edge: %+v", result.Edges)
	}
	reverse, err := s.Neighbors(t.Context(), "demo", dolmen.NodeRef{Table: "nodes", ID: 2}, dolmen.NeighborsOptions{Direction: "in"})
	if err != nil || !reflect.DeepEqual(result.Edges, reverse.Edges) {
		t.Fatalf("reverse: %+v %v", reverse, err)
	}
	filtered, err := s.Neighbors(t.Context(), "demo", dolmen.NodeRef{Table: "nodes", ID: 1}, dolmen.NeighborsOptions{RelationTypes: []string{"other"}})
	if err != nil || filtered.Truncated || len(filtered.Edges) != 0 || len(filtered.Nodes) != 1 {
		t.Fatalf("type filter: %+v %v", filtered, err)
	}
	bounded, err := s.Neighbors(t.Context(), "demo", dolmen.NodeRef{Table: "nodes", ID: 1}, dolmen.NeighborsOptions{MaxNodes: 1})
	if err != nil || !bounded.Truncated || bounded.StopReason != "nodes" || len(bounded.Edges) != 0 || len(bounded.Nodes) != 1 {
		t.Fatalf("bounded: %+v %v", bounded, err)
	}
}

func TestNeighborsPublicReadLifecycle(t *testing.T) {
	s, err := dolmen.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Neighbors(t.Context(), "missing", dolmen.NodeRef{Table: "nodes", ID: 1}, dolmen.NeighborsOptions{}); !errors.Is(err, dolmen.ErrNotFound) {
		t.Fatalf("missing namespace: %v", err)
	}
	names, err := s.ListNamespaces(t.Context(), dolmen.ListNamespacesOptions{})
	if err != nil || len(names) != 0 {
		t.Fatalf("read created namespace: %v %v", names, err)
	}
	if _, err := s.Neighbors(t.Context(), "missing", dolmen.NodeRef{Table: "bad-name", ID: 1}, dolmen.NeighborsOptions{}); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("bad table: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Neighbors(t.Context(), "missing", dolmen.NodeRef{Table: "nodes", ID: 1}, dolmen.NeighborsOptions{}); !errors.Is(err, dolmen.ErrClosed) {
		t.Fatalf("closed store: %v", err)
	}
}
