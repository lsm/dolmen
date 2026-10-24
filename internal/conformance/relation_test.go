package conformance

import (
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestRelationTablesAreSQLiteOnlyForNow(t *testing.T) {
	h := newHarness(t)
	h.ensureNS("acme")
	ctx := t.Context()
	if _, err := h.st.CreateTable(ctx, "acme", "nodes", []schema.Field{{Name: "label", Type: schema.String}}, store.TableOpts{AcceptReferences: testEngine(t) == store.EngineSQLite}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "src", Type: schema.Number, Required: true}, {Name: "dst", Type: schema.Number, Required: true}, {Name: "kind", Type: schema.String, Required: true}}
	opts := store.TableOpts{
		Constraints: []schema.Constraint{
			{Name: "src_ref", Kind: schema.ConstraintForeignKey, Field: "src", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
			{Name: "dst_ref", Kind: schema.ConstraintForeignKey, Field: "dst", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
		},
		Relation: &schema.Relation{Source: "src", Target: "dst", Type: "kind"},
	}
	sc, err := h.st.CreateTable(ctx, "acme", "links", fields, opts, [16]byte{})
	if testEngine(t) == store.EngineSQLite {
		if err != nil || sc.Relation == nil {
			t.Fatalf("SQLite relation table: %+v %v", sc, err)
		}
		return
	}
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("%s accepted a relation declaration it does not keep: %v", testEngine(t), err)
	}
	if _, _, err := h.st.DescribeTable(ctx, "acme", "links", nil, store.Incarnation{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("%s left a refused relation table behind: %v", testEngine(t), err)
	}
}
