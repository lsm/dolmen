package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/schema"
)

func graphQuery(table string, id int64) graph.Query {
	return graph.Query{Node: graph.Ref{Table: table, ID: id}, Direction: "both", Limits: graph.Limits{Nodes: 20, Edges: 20, Elapsed: time.Second}}
}
func seedGraph(t *testing.T) *Store {
	t.Helper()
	s := relationStore(t)
	for _, table := range []string{"alpha", "beta"} {
		if err := relInsert(t, s, table, map[string]any{"label": "one"}, map[string]any{"label": "two"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := relInsert(t, s, "links", map[string]any{"src": 1, "dst": 1, "kind": "k"}, map[string]any{"src": 2, "dst": 2, "kind": "k"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGraphSQLiteReadsOnePinnedSnapshotDuringWrites(t *testing.T) {
	s := seedGraph(t)
	ctx := t.Context()
	index := s.GraphIndex(nil)
	view, err := index.Snapshot(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if err := relInsert(t, s, "links", map[string]any{"src": 1, "dst": 2, "kind": "new"}); err != nil {
		t.Fatal(err)
	}
	old, more, err := view.Adjacent(ctx, graph.Ref{Table: "alpha", ID: 1}, "out", 10)
	if err != nil || more || len(old) != 1 || old[0].Ref.ID != 1 {
		t.Fatalf("snapshot mixed commits: %+v %v %v", old, more, err)
	}
	fresh, err := graph.Neighbors(ctx, index, "ns", graphQuery("alpha", 1))
	if err != nil || len(fresh.Edges) != 2 {
		t.Fatalf("new snapshot lacks committed edge: %+v %v", fresh, err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGraphSQLiteForwardReverseAndAtomicIndexMaintenance(t *testing.T) {
	s := seedGraph(t)
	ctx := t.Context()
	index := s.GraphIndex(nil)
	out, err := graph.Neighbors(ctx, index, "ns", graphQuery("alpha", 1))
	if err != nil || len(out.Edges) != 1 || out.Edges[0].Target != (graph.Ref{Table: "beta", ID: 1}) {
		t.Fatalf("forward %+v %v", out, err)
	}
	in := graphQuery("beta", 1)
	in.Direction = "in"
	reverse, err := graph.Neighbors(ctx, index, "ns", in)
	if err != nil || !reflect.DeepEqual(out.Edges, reverse.Edges) {
		t.Fatalf("reverse %+v %v", reverse, err)
	}
	if _, err := s.Update(ctx, "ns", "links", "id = ?", []any{1}, map[string]any{"dst": 2}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	empty, err := graph.Neighbors(ctx, index, "ns", in)
	if err != nil || len(empty.Edges) != 0 {
		t.Fatalf("stale reverse adjacency %+v %v", empty, err)
	}
	n, err := s.ns("ns")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	for _, field := range []string{"src", "dst"} {
		rows, err := n.ro.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT id FROM links WHERE "+field+"=? ORDER BY id LIMIT 10", 1)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !strings.Contains(plan, "SEARCH") || !strings.Contains(plan, "INDEX") {
			t.Fatalf("%s is not indexed: %s", field, plan)
		}
	}
}

func TestGraphSQLiteAppliesEndpointAndEdgeTableVisibility(t *testing.T) {
	s := seedGraph(t)
	ctx := t.Context()
	for _, hidden := range []string{"links", "beta", "alpha"} {
		index := s.GraphIndex(func(ctx context.Context, sc *schema.TableSchema) (*RowScope, error) {
			if sc.Name == hidden {
				return &RowScope{Empty: true}, nil
			}
			return nil, nil
		})
		got, err := graph.Neighbors(ctx, index, "ns", graphQuery("alpha", 1))
		if err != nil || len(got.Edges) != 0 {
			t.Fatalf("hidden %s leaked %+v %v", hidden, got, err)
		}
		if hidden == "alpha" && len(got.Nodes) != 0 {
			t.Fatalf("hidden seed leaked %+v", got)
		}
		for _, ref := range got.Nodes {
			if ref.Table == hidden {
				t.Fatalf("hidden endpoint leaked %+v", got)
			}
		}
	}
}

func TestGraphSQLiteUsesOwnerScopeForEveryRow(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	if _, err := s.CreateTable(ctx, "ns", "nodes", []schema.Field{{Name: "label", Type: schema.String}}, TableOpts{RowAccess: "own", AcceptReferences: true}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	opts := relationOpts("nodes", "nodes")
	opts.RowAccess = "own"
	if _, err := s.CreateTable(ctx, "ns", "links", relationFields(), opts, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		ins, err := s.Insert(ctx, "ns", "nodes", []map[string]any{{"label": "a"}, {"label": "b"}}, WriteOpts{Owner: owner}, Embedder{}, nil, Incarnation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Insert(ctx, "ns", "links", []map[string]any{{"src": ins.Ids[0], "dst": ins.Ids[1], "kind": "k"}}, WriteOpts{Owner: owner}, Embedder{}, nil, Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	index := s.GraphIndex(func(context.Context, *schema.TableSchema) (*RowScope, error) { return &RowScope{Owner: "alice"}, nil })
	got, err := graph.Neighbors(ctx, index, "ns", graphQuery("nodes", 1))
	if err != nil || len(got.Edges) != 1 || len(got.Nodes) != 2 {
		t.Fatalf("own rows %+v %v", got, err)
	}
	hidden, err := graph.Neighbors(ctx, index, "ns", graphQuery("nodes", 3))
	if err != nil || len(hidden.Nodes) != 0 || len(hidden.Edges) != 0 {
		t.Fatalf("other owner %+v %v", hidden, err)
	}
}

func TestGraphSQLiteRetainsDataAcrossReopenAndRefusesAmbiguousRoles(t *testing.T) {
	s := seedGraph(t)
	ctx := t.Context()
	dir := s.dir
	if _, err := graph.Neighbors(ctx, s.GraphIndex(nil), "ns", graphQuery("alpha", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := graph.Neighbors(ctx, reopened.GraphIndex(nil), "ns", graphQuery("alpha", 1))
	if err != nil || len(got.Edges) != 1 {
		t.Fatalf("reopened graph %+v %v", got, err)
	}
	opts := relationOpts("alpha", "beta")
	opts.Constraints = append(opts.Constraints, schema.Constraint{Name: "ambiguous_src", Kind: schema.ConstraintForeignKey, Field: "src", References: &schema.ConstraintReference{Table: "beta", Field: "id"}})
	if _, err := reopened.CreateTable(ctx, "ns", "ambiguous", relationFields(), opts, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Neighbors(ctx, reopened.GraphIndex(nil), "ns", graphQuery("alpha", 1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ambiguous endpoint silently selected: %v", err)
	}
}

func TestGraphSQLiteReadRefusesMissingNamespaceAndCanceledWork(t *testing.T) {
	s := seedGraph(t)
	if _, err := s.GraphIndex(nil).Snapshot(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing namespace: %v", err)
	}
	names, err := s.ListNamespaces(t.Context(), "", nil)
	if err != nil || !reflect.DeepEqual(names, []string{"ns"}) {
		t.Fatalf("read created namespace: %v %v", names, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.GraphIndex(nil).Snapshot(ctx, "ns"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot: %v", err)
	}
	failure := errors.New("synthetic access failure")
	if _, err := s.GraphIndex(func(context.Context, *schema.TableSchema) (*RowScope, error) { return nil, failure }).Snapshot(t.Context(), "ns"); !errors.Is(err, failure) {
		t.Fatalf("access failure: %v", err)
	}
	if _, err := graph.Neighbors(t.Context(), s.GraphIndex(nil), "ns", graphQuery("alpha", 1)); err != nil {
		t.Fatalf("failed open leaked resources: %v", err)
	}
}

func TestGraphSQLiteIndexNamesDoNotCollideAcrossTableFieldBoundaries(t *testing.T) {
	s := seedGraph(t)
	ctx := t.Context()
	for _, spec := range []struct{ table, field string }{{"a", "b_c"}, {"a_b", "c"}} {
		fields := relationFields()
		fields[0].Name = spec.field
		opts := relationOpts("alpha", "beta")
		opts.Relation.Source = spec.field
		opts.Constraints[0].Field = spec.field
		if _, err := s.CreateTable(ctx, "ns", spec.table, fields, opts, [16]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := graph.Neighbors(ctx, s.GraphIndex(nil), "ns", graphQuery("alpha", 1)); err != nil {
		t.Fatal(err)
	}
	n, err := s.ns("ns")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	for _, spec := range []struct{ table, field string }{{"a", "b_c"}, {"a_b", "c"}} {
		rows, err := n.ro.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT id FROM "+q(spec.table)+" WHERE "+q(spec.field)+"=1 ORDER BY id LIMIT 1")
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !strings.Contains(plan, "SEARCH") || !strings.Contains(plan, "INDEX") {
			t.Fatalf("%s.%s lost its index to a naming collision: %s", spec.table, spec.field, plan)
		}
	}
}
