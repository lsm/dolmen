package lakehouse

import (
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func lakeFormat(t *testing.T, s *Store) int {
	t.Helper()
	var format int
	if err := s.withNamespace(t.Context(), "app", func(n *namespace) error {
		return n.db.QueryRowContext(t.Context(), `SELECT format FROM _dolmen_lakehouse_meta WHERE singleton = 1`).Scan(&format)
	}); err != nil {
		t.Fatal(err)
	}
	return format
}

func TestLakehouseAcceptsDeclarationsEnforcesThemAndRefusesLifecycleChanges(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, WithSharedFilter(true))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if v := lakeFormat(t, s); v != catalogFormat {
		t.Fatalf("a catalog without constraints must stay readable by older binaries: version %d", v)
	}
	if _, err := s.CreateTable(ctx, "app", "plain", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	ref := func(parent string) []schema.Constraint {
		return []schema.Constraint{{Name: "project_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: parent, Field: "id"}}}
	}
	taskFields := []schema.Field{{Name: "project_id", Type: schema.Number}, {Name: "slug", Type: schema.String}, {Name: "done", Type: schema.Boolean}, {Name: "note", Type: schema.String}}
	if _, err := s.CreateTable(ctx, "app", "tasks", taskFields, store.TableOpts{Constraints: ref("plain")}, [16]byte{}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "accept_references") {
		t.Fatalf("a reference to a table that has not opted in: %v", err)
	}
	if _, _, err := s.DescribeTable(ctx, "app", "tasks", nil, store.Incarnation{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused declaration left the table behind: %v", err)
	}
	if _, err := s.CreateTable(ctx, "app", "projects", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{AcceptReferences: true}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if v := lakeFormat(t, s); v != constraintCatalogFormat {
		t.Fatalf("a constrained table must keep older binaries out: version %d", v)
	}
	if err := s.evict("app"); err != nil {
		t.Fatal(err)
	}
	if v := lakeFormat(t, s); v != constraintCatalogFormat {
		t.Fatalf("reopening the namespace must never lower its catalog format: %d", v)
	}
	declared := append(ref("projects"), schema.Constraint{Name: "open_slug", Kind: schema.ConstraintUnique, Fields: []string{"slug"}, Where: "done = FALSE"})
	if _, err := s.CreateTable(ctx, "app", "tasks", taskFields, store.TableOpts{Constraints: declared}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	insert := func(table string, rec map[string]any) error {
		_, err := s.Insert(ctx, "app", table, []map[string]any{rec}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
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
	if _, err := s.Delete(ctx, "app", "projects", "id = 1", nil, store.DeleteOpts{}, nil, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("deleting a referenced parent: %v", err)
	}
	if err := s.DropTable(ctx, "app", "projects", store.Incarnation{}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "tasks") {
		t.Fatalf("dropping a referenced table: %v", err)
	}
	for _, ch := range []schema.Change{
		{Op: schema.OpDropField, Name: "slug"},
		{Op: schema.OpRenameField, From: "done", To: "finished"},
	} {
		if _, err := s.Migrate(ctx, "app", "tasks", []schema.Change{ch}, store.Embedder{}, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "drop the constraint first") {
			t.Fatalf("%s: %v", ch.Op, err)
		}
	}
	if sc, err := s.Migrate(ctx, "app", "tasks", []schema.Change{{Op: schema.OpRenameField, From: "note", To: "memo"}}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil || len(sc.Constraints) != 2 {
		t.Fatalf("a migration must keep the table's declarations: %+v %v", sc, err)
	}
	on := true
	for _, table := range []string{"tasks", "projects"} {
		if _, err := s.Migrate(ctx, "app", table, []schema.Change{{Op: schema.OpSetRowAccess, Value: &on}}, store.Embedder{}, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "row_access") {
			t.Fatalf("set_row_access on %s: %v", table, err)
		}
	}
	if err := s.DropTable(ctx, "app", "tasks", store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropTable(ctx, "app", "projects", store.Incarnation{}); err != nil {
		t.Fatalf("a parent with no remaining references must drop: %v", err)
	}
}
