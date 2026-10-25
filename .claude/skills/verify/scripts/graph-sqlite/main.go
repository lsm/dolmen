package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
func query(id int64) graph.Query {
	return graph.Query{Node: graph.Ref{Table: "nodes", ID: id}, Direction: "both", Limits: graph.Limits{Nodes: 20, Edges: 20, Elapsed: time.Second}}
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
	_, err = s.CreateTable(ctx, "verify", "nodes", []schema.Field{{Name: "label", Type: schema.String}}, store.TableOpts{RowAccess: "own", AcceptReferences: true}, [16]byte{})
	must(err)
	fields := []schema.Field{{Name: "src", Type: schema.Number, Required: true}, {Name: "dst", Type: schema.Number, Required: true}, {Name: "kind", Type: schema.String, Required: true}}
	constraints := []schema.Constraint{
		{Name: "src_ref", Kind: schema.ConstraintForeignKey, Field: "src", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
		{Name: "dst_ref", Kind: schema.ConstraintForeignKey, Field: "dst", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
	}
	_, err = s.CreateTable(ctx, "verify", "links", fields, store.TableOpts{RowAccess: "own", Constraints: constraints, Relation: &schema.Relation{Source: "src", Target: "dst", Type: "kind"}}, [16]byte{})
	must(err)
	for _, owner := range []string{"alice", "bob"} {
		ins, err := s.Insert(ctx, "verify", "nodes", []map[string]any{{"label": "one"}, {"label": "two"}}, store.WriteOpts{Owner: owner}, store.Embedder{}, nil, store.Incarnation{})
		must(err)
		_, err = s.Insert(ctx, "verify", "links", []map[string]any{{"src": ins.Ids[0], "dst": ins.Ids[1], "kind": "knows"}}, store.WriteOpts{Owner: owner}, store.Embedder{}, nil, store.Incarnation{})
		must(err)
	}
	access := func(context.Context, *schema.TableSchema) (*store.RowScope, error) {
		return &store.RowScope{Owner: "alice"}, nil
	}
	index := s.GraphIndex(access)
	held, err := index.Snapshot(ctx, "verify")
	must(err)
	_, err = s.Insert(ctx, "verify", "links", []map[string]any{{"src": 2, "dst": 1, "kind": "returns"}}, store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{})
	must(err)
	old, more, err := held.Adjacent(ctx, query(1).Node, "both", 20)
	must(err)
	expect(len(old) == 1 && !more, "held snapshot mixed a later commit")
	fresh, err := graph.Neighbors(ctx, index, "verify", query(1))
	must(err)
	expect(len(fresh.Edges) == 2 && len(fresh.Nodes) == 2, "fresh snapshot missed committed edges")
	must(held.Close())
	fmt.Println("PASS held SQLite snapshot excludes concurrent commit; new snapshot sees it")
	hidden, err := graph.Neighbors(ctx, index, "verify", query(3))
	must(err)
	expect(len(hidden.Nodes) == 0 && len(hidden.Edges) == 0, "other owner's seed leaked")
	edgeScope := s.GraphIndex(func(_ context.Context, sc *schema.TableSchema) (*store.RowScope, error) {
		if sc.Name == "links" {
			return &store.RowScope{Owner: "alice"}, nil
		}
		return nil, nil
	})
	hidden, err = graph.Neighbors(ctx, edgeScope, "verify", query(3))
	must(err)
	expect(len(hidden.Nodes) == 1 && len(hidden.Edges) == 0, "other owner's edge leaked")
	denied := s.GraphIndex(func(_ context.Context, sc *schema.TableSchema) (*store.RowScope, error) {
		if sc.Name == "links" {
			return &store.RowScope{Empty: true}, nil
		}
		return nil, nil
	})
	hidden, err = graph.Neighbors(ctx, denied, "verify", query(1))
	must(err)
	expect(len(hidden.Nodes) == 1 && len(hidden.Edges) == 0, "denied edge table leaked")
	fmt.Println("PASS actual owner scopes and denied edge-table visibility suppress hidden rows")
	bounded := query(1)
	bounded.Limits.Edges = 1
	page, err := graph.Neighbors(ctx, index, "verify", bounded)
	must(err)
	expect(page.Truncated && page.StopReason == "edges" && len(page.Edges) == 1, "bounded adjacency claimed completeness")
	fmt.Println("PASS candidate cap returns explicit edges truncation")
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, "verify.db"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	must(err)
	for _, field := range []string{"src", "dst"} {
		rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT id FROM links WHERE "+field+"=1 ORDER BY id LIMIT 10")
		must(err)
		plan := ""
		for rows.Next() {
			var a, b, c int
			var detail string
			must(rows.Scan(&a, &b, &c, &detail))
			plan += detail
		}
		must(rows.Err())
		must(rows.Close())
		expect(strings.Contains(plan, "SEARCH") && strings.Contains(plan, "INDEX"), "adjacency lacks indexed search")
	}
	must(db.Close())
	fmt.Println("PASS forward and reverse SQLite plans use indexed SEARCH")
	_, err = s.Update(ctx, "verify", "links", "id = ?", []any{1}, map[string]any{"dst": 1}, store.Embedder{}, nil, store.Incarnation{})
	must(err)
	updated, err := graph.Neighbors(ctx, index, "verify", query(2))
	must(err)
	expect(len(updated.Edges) == 1 && updated.Edges[0].Ref.ID == 3, "index retained old endpoint after ordinary update")
	fmt.Println("PASS ordinary table update changes adjacency atomically")
	before, err := graph.Neighbors(ctx, index, "verify", query(1))
	must(err)
	records, err := s.Query(ctx, "verify", "SELECT id,src,dst,kind FROM links ORDER BY id", nil, [16]byte{}, store.Page{})
	must(err)
	must(s.Close())
	u.RawQuery = "mode=rw"
	db, err = sql.Open("sqlite", u.String())
	must(err)
	_, err = db.ExecContext(ctx, `DROP INDEX "_dolmen_graph_links.src"; DROP INDEX "_dolmen_graph_links.dst"`)
	must(err)
	must(db.Close())
	reopened, err := store.Open(dir)
	must(err)
	defer reopened.Close()
	after, err := graph.Neighbors(ctx, reopened.GraphIndex(access), "verify", query(1))
	must(err)
	expect(reflect.DeepEqual(before, after), "reopen/rebuild changed authoritative data")
	retained, err := reopened.Query(ctx, "verify", "SELECT id,src,dst,kind FROM links ORDER BY id", nil, [16]byte{}, store.Page{})
	must(err)
	expect(reflect.DeepEqual(records.Rows, retained.Rows), "index rebuild changed another owner’s rows")
	fmt.Println("PASS reopen rebuilds missing derived indexes without changing authoritative rows")
}
