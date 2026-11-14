package conformance

import (
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestRelationTablesAreRefusedOnLakehouseOnly(t *testing.T) {
	h := newHarness(t)
	h.ensureNS("acme")
	ctx := t.Context()
	if _, err := h.st.CreateTable(ctx, "acme", "nodes", []schema.Field{{Name: "label", Type: schema.String}}, store.TableOpts{AcceptReferences: true}, [16]byte{}); err != nil {
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
	if testEngine(t) != store.EngineLakehouse {
		if err != nil || sc.Relation == nil {
			t.Fatalf("%s relation table: %+v %v", testEngine(t), sc, err)
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

func TestRelationRoleFieldsStayLockedThroughMigrations(t *testing.T) {
	h := newHarness(t)
	relationTables(t, h)
	ctx := t.Context()
	for label, c := range map[string]struct {
		change schema.Change
		want   string
	}{
		"rename source":   {schema.Change{Op: schema.OpRenameField, From: "src", To: "origin"}, "src_ref"},
		"rename type":     {schema.Change{Op: schema.OpRenameField, From: "kind", To: "label"}, "relation"},
		"drop type":       {schema.Change{Op: schema.OpDropField, Name: "kind"}, "relation"},
		"drop target key": {schema.Change{Op: schema.OpDropConstraint, Name: "dst_ref"}, "relation"},
	} {
		if _, err := h.st.Migrate(ctx, "acme", "links", []schema.Change{c.change}, store.Embedder{}, store.Incarnation{Version: 1}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s %s: %v, want a refusal naming %s", testEngine(t), label, err, c.want)
		}
	}
	if _, err := h.st.Migrate(ctx, "acme", "links", []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "note", Type: schema.String}}}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	sc, _, err := h.st.DescribeTable(ctx, "acme", "links", nil, store.Incarnation{})
	if err != nil || sc.Relation == nil || sc.Relation.Type != "kind" {
		t.Fatalf("%s: a migration dropped the relation: %+v %v", testEngine(t), sc, err)
	}
	if err := h.st.DropTable(ctx, "acme", "nodes", store.Incarnation{}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "links") {
		t.Fatalf("%s: dropping an endpoint table: %v, want a refusal naming links", testEngine(t), err)
	}
}
