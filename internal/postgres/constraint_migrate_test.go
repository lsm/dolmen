package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresConstraintMigrationsCheckExistingRows(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "projects", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "tasks", []schema.Field{{Name: "project_id", Type: schema.Number}, {Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "projects", []map[string]any{{"slug": "p"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "tasks", []map[string]any{{"project_id": 1, "slug": "dup"}, {"project_id": 1, "slug": "dup"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	migrate := func(table string, ch schema.Change) (*schema.TableSchema, error) {
		return s.Migrate(ctx, "ns", table, []schema.Change{ch}, store.Embedder{}, store.Incarnation{})
	}
	unique := &schema.Constraint{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpAddConstraint, Constraint: unique}); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "slug_key") {
		t.Fatalf("adding a unique constraint existing rows break: %v", err)
	}
	if sc, _, err := s.DescribeTable(ctx, "ns", "tasks", nil, store.Incarnation{}); err != nil || len(sc.Constraints) != 0 || sc.Version != 1 {
		t.Fatalf("a refused add_constraint changed the schema: %+v %v", sc, err)
	}
	if v := pgCatalogVersion(t, s); v != catalogVersion {
		t.Fatalf("a refused add_constraint raised the catalog version to %d", v)
	}
	if _, err := s.Delete(ctx, "ns", "tasks", "id = 2", nil, store.DeleteOpts{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if sc, err := migrate("tasks", schema.Change{Op: schema.OpAddConstraint, Constraint: unique}); err != nil || len(sc.Constraints) != 1 {
		t.Fatalf("adding a unique constraint existing rows meet: %+v %v", sc, err)
	}
	if v := pgCatalogVersion(t, s); v != constraintCatalogVersion {
		t.Fatalf("add_constraint must keep older binaries out: catalog version %d", v)
	}
	fk := &schema.Constraint{Name: "project_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: "projects", Field: "id"}}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpAddConstraint, Constraint: fk}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "accept_references") {
		t.Fatalf("a foreign key to a table that has not opted in: %v", err)
	}
	on, off := true, false
	if _, err := migrate("projects", schema.Change{Op: schema.OpSetAcceptReferences, Value: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpAddConstraint, Constraint: fk}); err != nil {
		t.Fatalf("a foreign key every existing row meets: %v", err)
	}
	if _, err := migrate("projects", schema.Change{Op: schema.OpSetAcceptReferences, Value: &off}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "tasks") {
		t.Fatalf("disabling accept_references while referenced: %v", err)
	}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpDropConstraint, Name: "absent"}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("dropping an unknown constraint: %v", err)
	}
	if _, err := migrate("tasks", schema.Change{Op: schema.OpSetEnum, Name: "slug", Enum: &[]string{"dup"}, Constraint: unique}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "constraint is required on add_constraint") {
		t.Fatalf("a constraint on another op: %v", err)
	}
	mixed := []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "code", Type: schema.String}}, {Op: schema.OpAddConstraint, Constraint: &schema.Constraint{Name: "code_key", Kind: schema.ConstraintUnique, Fields: []string{"code"}}}}
	if _, err := s.Migrate(ctx, "ns", "tasks", mixed, store.Embedder{}, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "its own migrate call") {
		t.Fatalf("add_constraint sharing a migration with add_field: %v", err)
	}
	if sc, err := migrate("tasks", schema.Change{Op: schema.OpDropConstraint, Name: "project_ref"}); err != nil || len(sc.Constraints) != 1 {
		t.Fatalf("drop_constraint: %+v %v", sc, err)
	}
	if _, err := migrate("projects", schema.Change{Op: schema.OpSetAcceptReferences, Value: &off}); err != nil {
		t.Fatalf("disabling accept_references once nothing references the table: %v", err)
	}
	if _, err := s.Insert(ctx, "ns", "tasks", []map[string]any{{"project_id": 99, "slug": "fresh"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("a dropped foreign key still applied: %v", err)
	}
	if _, err := s.Insert(ctx, "ns", "tasks", []map[string]any{{"slug": "dup"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("the remaining unique constraint must still apply: %v", err)
	}
}

func TestPostgresAddConstraintDryRunReportsExistingViolations(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "notes", []map[string]any{{"slug": "dup"}, {"slug": "dup"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	add := []schema.Change{{Op: schema.OpAddConstraint, Constraint: &schema.Constraint{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}}
	if _, err := s.PlanMigration(ctx, "ns", "notes", add, store.Embedder{}, store.Incarnation{}, nil, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "slug_key") {
		t.Fatalf("dry run must report existing violations: %v", err)
	}
	if _, err := s.Delete(ctx, "ns", "notes", "id = 2", nil, store.DeleteOpts{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanMigration(ctx, "ns", "notes", add, store.Embedder{}, store.Incarnation{}, nil, store.Incarnation{})
	if err != nil || !plan.DryRun || len(plan.Table.Constraints) != 1 {
		t.Fatalf("feasible dry run: %+v %v", plan, err)
	}
	sc, _, err := s.DescribeTable(ctx, "ns", "notes", nil, store.Incarnation{})
	if err != nil || sc.Version != 1 || len(sc.Constraints) != 0 {
		t.Fatalf("dry run changed schema: %+v %v", sc, err)
	}
	if v := pgCatalogVersion(t, s); v != catalogVersion {
		t.Fatalf("dry run changed reader version: %d", v)
	}
	if _, err := s.Insert(ctx, "ns", "notes", []map[string]any{{"slug": "dup"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("dry run enforced a constraint: %v", err)
	}
	if _, err := s.Migrate(ctx, "ns", "notes", add, store.Embedder{}, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("execution must recheck rows after a successful dry run: %v", err)
	}
}
