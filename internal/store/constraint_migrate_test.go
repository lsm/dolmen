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

func TestSQLiteAddConstraintDryRunReportsExistingViolations(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	if _, err := s.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "notes", []map[string]any{{"slug": "dup"}, {"slug": "dup"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	add := []schema.Change{{Op: schema.OpAddConstraint, Constraint: &schema.Constraint{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}}
	if _, err := s.PlanMigration(ctx, "ns", "notes", add, Embedder{}, Incarnation{}, nil, Incarnation{}); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "slug_key") {
		t.Fatalf("a dry run must report that existing rows break the constraint: %v", err)
	}
}

func TestSQLiteConstraintChangesRunInTheirOwnMigration(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	unique := []schema.Constraint{{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}
	if _, err := s.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "slug", Type: schema.String}, {Name: "memo", Type: schema.String}}, TableOpts{AcceptReferences: true, Constraints: unique}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	on := true
	for _, constraintOp := range []schema.Change{{Op: schema.OpDropConstraint, Name: "slug_key"}, {Op: schema.OpSetAcceptReferences, Value: &on}} {
		for _, fieldOp := range []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "code", Type: schema.String}}, {Op: schema.OpRenameField, From: "memo", To: "note"}, {Op: schema.OpSetRowAccess, Value: &on}} {
			_, err := s.Migrate(ctx, "ns", "notes", []schema.Change{constraintOp, fieldOp}, Embedder{}, Incarnation{Version: 1})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "its own migrate call") {
				t.Fatalf("%s with %s: %v", constraintOp.Op, fieldOp.Op, err)
			}
		}
	}
}

func TestDropConstraintKeepsTheForeignKeysARelationUses(t *testing.T) {
	s := relationStore(t)
	ctx := t.Context()
	for _, name := range []string{"src_ref", "dst_ref"} {
		_, err := s.Migrate(ctx, "ns", "links", []schema.Change{{Op: schema.OpDropConstraint, Name: name}}, Embedder{}, Incarnation{})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "relation") {
			t.Fatalf("dropping %s, which the relation's endpoint uses: %v", name, err)
		}
	}
	sc, _, err := s.DescribeTable(ctx, "ns", "links", nil, Incarnation{})
	if err != nil || len(sc.Constraints) != 2 {
		t.Fatalf("a refused drop_constraint changed the schema: %+v %v", sc, err)
	}
}

func TestSQLiteConstraintOperationsRunAloneAcrossChanges(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	if _, err := s.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{Constraints: []schema.Constraint{{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	on := true
	add := schema.Constraint{Name: "another_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}
	for _, constraintOp := range []schema.Change{{Op: schema.OpAddConstraint, Constraint: &add}, {Op: schema.OpDropConstraint, Name: "slug_key"}, {Op: schema.OpSetAcceptReferences, Value: &on}} {
		for _, other := range []schema.Change{{Op: schema.OpSetEnum, Name: "slug", Enum: &[]string{"a"}}, {Op: schema.OpSetFulltext, Name: "slug", Value: &on}, {Op: schema.OpSetRowAccess, Value: &on}, {Op: schema.OpSetAcceptReferences, Value: &on}} {
			t.Run(constraintOp.Op+"/"+other.Op, func(t *testing.T) {
				_, err := s.PlanMigration(ctx, "ns", "notes", []schema.Change{constraintOp, other}, Embedder{}, Incarnation{Version: 1}, nil, Incarnation{})
				if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "its own migrate call") {
					t.Fatalf("constraint operation must run alone: %v", err)
				}
			})
		}
	}
}

func TestSQLiteAcceptReferencesRequiresValue(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	if _, err := s.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Migrate(ctx, "ns", "notes", []schema.Change{{Op: schema.OpSetAcceptReferences}}, Embedder{}, Incarnation{})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "explicit value") {
		t.Fatalf("missing value: %v", err)
	}
}
