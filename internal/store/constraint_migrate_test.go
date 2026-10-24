package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func TestSQLiteConstraintMigrationsCheckExistingRows(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	if _, err := s.CreateTable(ctx, "ns", "projects", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "tasks", []schema.Field{{Name: "project_id", Type: schema.Number}, {Name: "slug", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "projects", []map[string]any{{"slug": "p"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "tasks", []map[string]any{{"project_id": 1, "slug": "dup"}, {"project_id": 1, "slug": "dup"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	migrate := func(table string, ch schema.Change) (*schema.TableSchema, error) {
		return s.Migrate(ctx, "ns", table, []schema.Change{ch}, Embedder{}, Incarnation{})
	}
	unique := &schema.Constraint{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpAddConstraint, Constraint: unique}); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "slug_key") {
		t.Fatalf("adding a unique constraint existing rows break: %v", err)
	}
	if sc, _, err := s.DescribeTable(ctx, "ns", "tasks", nil, Incarnation{}); err != nil || len(sc.Constraints) != 0 || sc.Version != 1 {
		t.Fatalf("a refused add_constraint changed the schema: %+v %v", sc, err)
	}
	if minReader := catalogMinReader(t, s, "ns"); minReader >= ConstraintMinReader {
		t.Fatalf("a refused add_constraint stamped catalog_min_reader %d", minReader)
	}
	if _, err := s.Delete(ctx, "ns", "tasks", "id = 2", nil, DeleteOpts{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if sc, err := migrate("tasks", schema.Change{Op: schema.OpAddConstraint, Constraint: unique}); err != nil || len(sc.Constraints) != 1 {
		t.Fatalf("adding a unique constraint existing rows meet: %+v %v", sc, err)
	}
	if minReader := catalogMinReader(t, s, "ns"); minReader != ConstraintMinReader {
		t.Fatalf("add_constraint must keep older binaries out: catalog_min_reader %d", minReader)
	}
	fk := &schema.Constraint{Name: "project_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: "projects", Field: "id"}}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpAddConstraint, Constraint: fk}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "accept_references") {
		t.Fatalf("a foreign key to a table that has not opted in: %v", err)
	}
	on, off := true, false
	if _, err := migrate("projects", schema.Change{Op: schema.OpSetAcceptReferences, Value: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpAddConstraint, Constraint: fk}); err != nil {
		t.Fatalf("a foreign key every existing row meets: %v", err)
	}
	if _, err := migrate("projects", schema.Change{Op: schema.OpSetAcceptReferences, Value: &off}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "tasks") {
		t.Fatalf("disabling accept_references while referenced: %v", err)
	}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpDropConstraint, Name: "absent"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dropping an unknown constraint: %v", err)
	}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpSetEnum, Name: "slug", Enum: &[]string{"dup"}, Constraint: unique}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "constraint is required on add_constraint") {
		t.Fatalf("a constraint on another op: %v", err)
	}
	mixed := []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "code", Type: schema.String}}, {Op: schema.OpAddConstraint, Constraint: &schema.Constraint{Name: "code_key", Kind: schema.ConstraintUnique, Fields: []string{"code"}}}}
	if _, err := s.Migrate(ctx, "ns", "tasks", mixed, Embedder{}, Incarnation{}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "its own migrate call") {
		t.Fatalf("add_constraint sharing a migration with add_field: %v", err)
	}
	if sc, err := migrate("tasks", schema.Change{Op: schema.OpDropConstraint, Name: "project_ref"}); err != nil || len(sc.Constraints) != 1 {
		t.Fatalf("drop_constraint: %+v %v", sc, err)
	}
	if _, err := migrate("projects", schema.Change{Op: schema.OpSetAcceptReferences, Value: &off}); err != nil {
		t.Fatalf("disabling accept_references once nothing references the table: %v", err)
	}
	if _, err := s.Insert(ctx, "ns", "tasks", []map[string]any{{"project_id": 99, "slug": "fresh"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("a dropped foreign key still applied: %v", err)
	}
	if _, err := s.Insert(ctx, "ns", "tasks", []map[string]any{{"slug": "dup"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("the remaining unique constraint must still apply: %v", err)
	}
}

func TestSQLiteAcceptReferencesCannotBeTurnedOffUnderASelfReference(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	self := []schema.Constraint{{Name: "parent_ref", Kind: schema.ConstraintForeignKey, Field: "parent_id", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}}}
	if _, err := s.CreateTable(ctx, "ns", "nodes", []schema.Field{{Name: "parent_id", Type: schema.Number}}, TableOpts{AcceptReferences: true, Constraints: self}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	off := false
	_, err := s.Migrate(ctx, "ns", "nodes", []schema.Change{{Op: schema.OpSetAcceptReferences, Value: &off}}, Embedder{}, Incarnation{})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "drop that constraint first") || !strings.Contains(err.Error(), "nodes") {
		t.Fatalf("turning off accept_references under the table's own foreign key: %v", err)
	}
}
