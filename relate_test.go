package dolmen

import (
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestRelateInsertsOneIdempotentEdge(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := t.Context()
	if err := st.CreateNamespace(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.eng.CreateTable(ctx, "ns", "nodes", []schema.Field{{Name: "label", Type: schema.String}}, store.TableOpts{AcceptReferences: true}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "src", Type: schema.Number, Required: true}, {Name: "dst", Type: schema.Number, Required: true}, {Name: "kind", Type: schema.String, Required: true}, {Name: "weight", Type: schema.Number}}
	opts := store.TableOpts{
		Constraints: []schema.Constraint{
			{Name: "src_ref", Kind: schema.ConstraintForeignKey, Field: "src", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
			{Name: "dst_ref", Kind: schema.ConstraintForeignKey, Field: "dst", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
		},
		Relation: &schema.Relation{Source: "src", Target: "dst", Type: "kind"},
	}
	if _, err := st.eng.CreateTable(ctx, "ns", "links", fields, opts, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "ns", "nodes", []map[string]any{{"label": "a"}, {"label": "b"}}, InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	edge := Edge{Source: 1, Target: 2, Type: "k", Properties: map[string]any{"weight": 0.5}}
	r, err := st.Relate(ctx, "ns", "links", edge, InsertOptions{IdempotencyKey: "e1"})
	if err != nil || r.ID != 1 || r.Replayed {
		t.Fatalf("relate: %+v %v", r, err)
	}
	if r, err = st.Relate(ctx, "ns", "links", edge, InsertOptions{IdempotencyKey: "e1"}); err != nil || r.ID != 1 || !r.Replayed {
		t.Fatalf("replay: %+v %v", r, err)
	}
	if _, err := st.Relate(ctx, "ns", "links", Edge{Source: 1, Target: 2, Type: "other"}, InsertOptions{IdempotencyKey: "e1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a reused key for another edge: %v", err)
	}
	if _, err := st.Relate(ctx, "ns", "links", Edge{Source: 1, Target: 9, Type: "k"}, InsertOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a missing endpoint: %v", err)
	}
	if _, err := st.Relate(ctx, "ns", "nodes", Edge{Source: 1, Target: 2, Type: "k"}, InsertOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a table that is not a relation: %v", err)
	}
	if _, err := st.Relate(ctx, "ns", "links", Edge{Source: 1, Target: 2, Type: "k", Properties: map[string]any{"dst": 1}}, InsertOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a role field in properties: %v", err)
	}
	rows, err := st.GetRows(ctx, "ns", "links", []int64{1})
	if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["kind"] != "k" {
		t.Fatalf("stored edge: %+v %v", rows, err)
	}
}
