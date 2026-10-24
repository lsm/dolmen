package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func relationFields() []schema.Field {
	return []schema.Field{
		{Name: "src", Type: schema.Number, Required: true},
		{Name: "dst", Type: schema.Number, Required: true},
		{Name: "kind", Type: schema.String, Required: true},
		{Name: "weight", Type: schema.Number},
	}
}

func relationRefs(source, target string) []schema.Constraint {
	return []schema.Constraint{
		{Name: "src_ref", Kind: schema.ConstraintForeignKey, Field: "src", References: &schema.ConstraintReference{Table: source, Field: "id"}},
		{Name: "dst_ref", Kind: schema.ConstraintForeignKey, Field: "dst", References: &schema.ConstraintReference{Table: target, Field: "id"}},
	}
}

func relationOpts(source, target string) TableOpts {
	return TableOpts{Constraints: relationRefs(source, target), Relation: &schema.Relation{Source: "src", Target: "dst", Type: "kind"}}
}

func relationStore(t *testing.T) *Store {
	t.Helper()
	s := openRowAccessStore(t)
	for _, table := range []string{"alpha", "beta"} {
		if _, err := s.CreateTable(t.Context(), "ns", table, []schema.Field{{Name: "label", Type: schema.String}}, TableOpts{AcceptReferences: true}, [16]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateTable(t.Context(), "ns", "links", relationFields(), relationOpts("alpha", "beta"), [16]byte{}); err != nil {
		t.Fatal(err)
	}
	return s
}

func relInsert(t *testing.T, s *Store, table string, recs ...map[string]any) error {
	t.Helper()
	_, err := s.Insert(t.Context(), "ns", table, recs, WriteOpts{}, Embedder{}, nil, Incarnation{})
	return err
}

func relCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	res, err := s.Query(t.Context(), "ns", "SELECT count(*) AS n FROM "+table, nil, [16]byte{}, Page{})
	if err != nil {
		t.Fatal(err)
	}
	n, _ := res.Rows[0]["n"].(int64)
	return int(n)
}

func TestSQLiteRelationDeclarationNamesRolesBackedByForeignKeys(t *testing.T) {
	s := relationStore(t)
	ctx := t.Context()
	sc, _, err := s.DescribeTable(ctx, "ns", "links", nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Relation == nil || *sc.Relation != (schema.Relation{Source: "src", Target: "dst", Type: "kind"}) {
		t.Fatalf("describe lost the relation declaration: %+v", sc.Relation)
	}
	optional := relationFields()
	optional[0].Required = false
	untyped := relationFields()
	untyped[2].Type = schema.Number
	for label, c := range map[string]struct {
		fields []schema.Field
		opts   TableOpts
		want   string
	}{
		"no foreign key on source": {relationFields(), TableOpts{Constraints: relationRefs("alpha", "beta")[1:], Relation: &schema.Relation{Source: "src", Target: "dst", Type: "kind"}}, "foreign_key"},
		"optional source":          {optional, relationOpts("alpha", "beta"), "required"},
		"numeric type":             {untyped, relationOpts("alpha", "beta"), "string"},
		"undeclared type":          {relationFields(), TableOpts{Constraints: relationRefs("alpha", "beta"), Relation: &schema.Relation{Source: "src", Target: "dst", Type: "missing"}}, "missing"},
		"one field, two roles":     {relationFields(), TableOpts{Constraints: relationRefs("alpha", "beta"), Relation: &schema.Relation{Source: "src", Target: "src", Type: "kind"}}, "distinct"},
	} {
		if _, err := s.CreateTable(ctx, "ns", "bad_links", c.fields, c.opts, [16]byte{}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v, want invalid naming %q", label, err, c.want)
		}
		if _, _, err := s.DescribeTable(ctx, "ns", "bad_links", nil, Incarnation{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: a refused declaration left the table behind: %v", label, err)
		}
	}
	for _, ch := range []schema.Change{{Op: schema.OpDropField, Name: "kind"}, {Op: schema.OpRenameField, From: "kind", To: "label"}} {
		if _, err := s.Migrate(ctx, "ns", "links", []schema.Change{ch}, Embedder{}, Incarnation{Version: 1}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "relation") {
			t.Fatalf("%s on the type field: %v, want a refusal naming the relation", ch.Op, err)
		}
	}
	if _, err := s.Migrate(ctx, "ns", "links", []schema.Change{{Op: schema.OpRenameField, From: "weight", To: "score"}}, Embedder{}, Incarnation{Version: 1}); err != nil {
		t.Fatalf("renaming a property field: %v", err)
	}
	if sc, _, err = s.DescribeTable(ctx, "ns", "links", nil, Incarnation{}); err != nil || sc.Relation == nil {
		t.Fatalf("a migration dropped the relation declaration: %+v %v", sc, err)
	}
}

func TestSQLiteRelationEndpointsAreValidatedOnEveryWrite(t *testing.T) {
	s := relationStore(t)
	ctx := t.Context()
	if err := relInsert(t, s, "alpha", map[string]any{"label": "a1"}); err != nil {
		t.Fatal(err)
	}
	if err := relInsert(t, s, "beta", map[string]any{"label": "b1"}); err != nil {
		t.Fatal(err)
	}
	if err := relInsert(t, s, "links", map[string]any{"src": 1, "dst": 1, "kind": "k", "weight": 0.5}); err != nil {
		t.Fatalf("an edge between existing rows: %v", err)
	}
	if err := relInsert(t, s, "links", map[string]any{"src": 1, "dst": 9, "kind": "k"}); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "dst_ref") {
		t.Fatalf("an edge to a missing target: %v", err)
	}
	if _, err := s.Update(ctx, "ns", "links", "id = 1", nil, map[string]any{"src": 9}, Embedder{}, nil, Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("moving an edge to a missing source: %v", err)
	}
	good := []BatchWrite{
		{Kind: BatchWriteInsert, Table: "beta", Records: []map[string]any{{"label": "b2"}}},
		{Kind: BatchWriteInsert, Table: "links", Records: []map[string]any{{"src": 1, "dst": 2, "kind": "k"}}},
	}
	if _, err := s.Batch(ctx, "ns", good, BatchOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("a node and its edge in one batch: %v", err)
	}
	bad := []BatchWrite{
		{Kind: BatchWriteInsert, Table: "beta", Records: []map[string]any{{"label": "b3"}}},
		{Kind: BatchWriteInsert, Table: "links", Records: []map[string]any{{"src": 7, "dst": 3, "kind": "k"}}},
	}
	if _, err := s.Batch(ctx, "ns", bad, BatchOpts{}, Embedder{}, nil, Incarnation{}); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "writes[1]") {
		t.Fatalf("a batch with a dangling edge: %v", err)
	}
	if relCount(t, s, "beta") != 2 || relCount(t, s, "links") != 2 {
		t.Fatal("a refused batch kept its node")
	}
}

func TestSQLiteRelationEdgesNeverReconnectAfterRowReuse(t *testing.T) {
	s := relationStore(t)
	ctx := t.Context()
	if err := relInsert(t, s, "alpha", map[string]any{"label": "a1"}); err != nil {
		t.Fatal(err)
	}
	if err := relInsert(t, s, "beta", map[string]any{"label": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := relInsert(t, s, "links", map[string]any{"src": 1, "dst": 1, "kind": "k"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, "ns", "beta", "id = 1", nil, DeleteOpts{}, nil, Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("deleting an edge's endpoint: %v, want conflict", err)
	}
	del := []BatchWrite{{Kind: BatchWriteDelete, Table: "beta", Filter: "id = 1"}}
	if _, err := s.Batch(ctx, "ns", del, BatchOpts{}, Embedder{}, nil, Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("deleting an edge's endpoint in a batch: %v, want conflict", err)
	}
	if _, err := s.Delete(ctx, "ns", "links", "id = 1", nil, DeleteOpts{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, "ns", "beta", "id = 1", nil, DeleteOpts{}, nil, Incarnation{}); err != nil {
		t.Fatalf("an endpoint with no edges left: %v", err)
	}
	if err := relInsert(t, s, "beta", map[string]any{"label": "new"}); err != nil {
		t.Fatal(err)
	}
	if relCount(t, s, "links") != 0 {
		t.Fatal("an edge outlived its endpoint and would attach to the reused id")
	}
}

func TestSQLiteRelationEdgesNeverReconnectAfterTableReuse(t *testing.T) {
	s := relationStore(t)
	ctx := t.Context()
	if err := s.DropTable(ctx, "ns", "beta", Incarnation{}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "links") {
		t.Fatalf("dropping an endpoint table of an empty relation: %v, want a refusal naming links", err)
	}
	if err := relInsert(t, s, "alpha", map[string]any{"label": "a1"}); err != nil {
		t.Fatal(err)
	}
	if err := relInsert(t, s, "beta", map[string]any{"label": "b1"}); err != nil {
		t.Fatal(err)
	}
	if err := relInsert(t, s, "links", map[string]any{"src": 1, "dst": 1, "kind": "k"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropTable(ctx, "ns", "links", Incarnation{}); err != nil {
		t.Fatalf("dropping the relation table: %v", err)
	}
	if err := s.DropTable(ctx, "ns", "beta", Incarnation{}); err != nil {
		t.Fatalf("an endpoint table nothing references: %v", err)
	}
	if _, err := s.CreateTable(ctx, "ns", "beta", []schema.Field{{Name: "label", Type: schema.String}}, TableOpts{AcceptReferences: true}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "links", relationFields(), relationOpts("alpha", "beta"), [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := relInsert(t, s, "beta", map[string]any{"label": "b1"}); err != nil {
		t.Fatal(err)
	}
	if relCount(t, s, "links") != 0 {
		t.Fatal("a recreated relation table brought back edges from its earlier incarnation")
	}
}
