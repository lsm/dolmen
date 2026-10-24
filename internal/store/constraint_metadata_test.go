package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func TestSQLiteCreateTableRefusesInvalidDeclarationsBeforeDDL(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	fields := []schema.Field{{Name: "slug", Type: schema.String}, {Name: "parent_id", Type: schema.Number}}
	if _, err := s.CreateTable(ctx, "ns", "plain_parent", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if minReader := catalogMinReader(t, s, "ns"); minReader >= ConstraintMinReader {
		t.Fatalf("a namespace without declarations stamped catalog_min_reader %d", minReader)
	}
	if _, err := s.CreateTable(ctx, "ns", "own_parent", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{RowAccess: schema.RowAccessOwn, AcceptReferences: true}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if minReader := catalogMinReader(t, s, "ns"); minReader != ConstraintMinReader {
		t.Fatalf("accept_references alone must keep older binaries out: catalog_min_reader %d", minReader)
	}
	fk := func(parent string) []schema.Constraint {
		return []schema.Constraint{{Name: "parent_ref", Kind: schema.ConstraintForeignKey, Field: "parent_id", References: &schema.ConstraintReference{Table: parent, Field: "id"}}}
	}
	for label, opts := range map[string]TableOpts{
		"malformed":         {Constraints: []schema.Constraint{{}}},
		"missing parent":    {Constraints: fk("absent")},
		"parent not opted":  {Constraints: fk("plain_parent")},
		"mixed row access":  {Constraints: fk("own_parent")},
		"self not opted in": {Constraints: fk("notes")},
	} {
		if _, err := s.CreateTable(ctx, "ns", "notes", fields, opts, [16]byte{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v, want invalid", label, err)
		}
		if _, _, err := s.DescribeTable(ctx, "ns", "notes", nil, Incarnation{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: a refused declaration left the table behind: %v", label, err)
		}
	}
	sc, err := s.CreateTable(ctx, "ns", "notes", fields, TableOpts{AcceptReferences: true, Constraints: fk("notes")}, [16]byte{})
	if err != nil {
		t.Fatalf("a self-reference on a table accepting references: %v", err)
	}
	if !sc.AcceptReferences || len(sc.Constraints) != 1 {
		t.Fatalf("created schema lost its declarations: %+v", sc)
	}
	if minReader := catalogMinReader(t, s, "ns"); minReader != ConstraintMinReader {
		t.Fatalf("a constrained table must keep older binaries out: catalog_min_reader %d, want %d", minReader, ConstraintMinReader)
	}
}

func catalogMinReader(t *testing.T, s *Store, ns string) int {
	t.Helper()
	n, err := s.ns(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	_, minReader, err := readCatalogVersion(t.Context(), n.rw)
	if err != nil {
		t.Fatal(err)
	}
	return minReader
}

func TestSQLiteEnforcesDeclaredConstraintsAndRefusesLifecycleChanges(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	if _, err := s.CreateTable(ctx, "ns", "projects", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{AcceptReferences: true}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	tasks := []schema.Constraint{
		{Name: "project_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: "projects", Field: "id"}},
		{Name: "open_slug", Kind: schema.ConstraintUnique, Fields: []string{"slug"}, Where: "done = FALSE"},
	}
	if _, err := s.CreateTable(ctx, "ns", "tasks", []schema.Field{{Name: "project_id", Type: schema.Number}, {Name: "slug", Type: schema.String}, {Name: "done", Type: schema.Boolean}, {Name: "note", Type: schema.String}}, TableOpts{Constraints: tasks}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	insert := func(table string, rec map[string]any) error {
		_, err := s.Insert(ctx, "ns", table, []map[string]any{rec}, WriteOpts{}, Embedder{}, nil, Incarnation{})
		return err
	}
	if err := insert("projects", map[string]any{"slug": "p"}); err != nil {
		t.Fatal(err)
	}
	if err := insert("tasks", map[string]any{"project_id": 1, "slug": "a", "done": false}); err != nil {
		t.Fatal(err)
	}
	for label, rec := range map[string]map[string]any{
		"open_slug":   {"project_id": 1, "slug": "a", "done": false},
		"project_ref": {"project_id": 2, "slug": "b", "done": false},
	} {
		if err := insert("tasks", rec); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), label) {
			t.Fatalf("%s: %v, want a conflict naming it", label, err)
		}
	}
	if err := insert("tasks", map[string]any{"project_id": 1, "slug": "a", "done": true}); err != nil {
		t.Fatalf("a row outside the predicate must not conflict: %v", err)
	}
	if _, err := s.Delete(ctx, "ns", "projects", "id = 1", nil, DeleteOpts{}, nil, Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("deleting a referenced parent: %v", err)
	}
	if err := s.DropTable(ctx, "ns", "projects", Incarnation{}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "tasks") {
		t.Fatalf("dropping a referenced table: %v", err)
	}
	for _, ch := range []schema.Change{
		{Op: schema.OpDropField, Name: "slug"},
		{Op: schema.OpRenameField, From: "done", To: "finished"},
		{Op: schema.OpDropField, Name: "project_id"},
	} {
		if _, err := s.Migrate(ctx, "ns", "tasks", []schema.Change{ch}, Embedder{}, Incarnation{}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "drop the constraint first") {
			t.Fatalf("%s %s%s: %v", ch.Op, ch.Name, ch.From, err)
		}
	}
	if _, err := s.Migrate(ctx, "ns", "tasks", []schema.Change{{Op: schema.OpRenameField, From: "note", To: "memo"}}, Embedder{}, Incarnation{Version: 1}); err != nil {
		t.Fatalf("renaming a field no constraint names: %v", err)
	}
	on := true
	for _, table := range []string{"tasks", "projects"} {
		if _, err := s.Migrate(ctx, "ns", table, []schema.Change{{Op: schema.OpSetRowAccess, Value: &on}}, Embedder{}, Incarnation{}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "row_access") {
			t.Fatalf("set_row_access on %s: %v", table, err)
		}
	}
	if err := s.DropTable(ctx, "ns", "tasks", Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropTable(ctx, "ns", "projects", Incarnation{}); err != nil {
		t.Fatalf("a parent with no remaining references must drop: %v", err)
	}
}
