package constraints

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func catalogFixture() map[string]Table {
	return map[string]Table{
		"projects": {Schema: &schema.TableSchema{Name: "projects", Namespace: "app", Fields: []schema.Field{{Name: "slug", Type: schema.String}}, AcceptReferences: true}, Rows: map[int64]Row{1: {Values: map[string]any{"slug": "one"}}}},
		"tasks": {Schema: &schema.TableSchema{Name: "tasks", Namespace: "app", Fields: []schema.Field{{Name: "project_id", Type: schema.Number}, {Name: "slug", Type: schema.String}, {Name: "active", Type: schema.Boolean}}, Constraints: []schema.Constraint{
			{Name: "project_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: "projects", Field: "id"}},
			{Name: "active_slug", Kind: schema.ConstraintUnique, Fields: []string{"slug"}, Where: "active = TRUE"},
		}}, Rows: map[int64]Row{1: {Values: map[string]any{"project_id": int64(1), "slug": "first", "active": true}}}},
	}
}

func TestCatalogChecksCompleteCandidateState(t *testing.T) {
	tables := catalogFixture()
	if err := CheckCatalog(t.Context(), tables); err != nil {
		t.Fatal(err)
	}
	delete(tables["projects"].Rows, 1)
	if err := CheckCatalog(t.Context(), tables); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("parent deletion: %v", err)
	}
	tables["tasks"].Rows[1].Values["project_id"] = nil
	if err := CheckCatalog(t.Context(), tables); err != nil {
		t.Fatal(err)
	}
	tables["tasks"].Rows[2] = Row{Values: map[string]any{"slug": "first", "active": false}}
	if err := CheckCatalog(t.Context(), tables); err != nil {
		t.Fatal(err)
	}
	tables["tasks"].Rows[2].Values["active"] = true
	if err := CheckCatalog(t.Context(), tables); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "active_slug") {
		t.Fatalf("duplicate transition: %v", err)
	}
	delete(tables["tasks"].Rows, 1)
	if err := CheckCatalog(t.Context(), tables); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogSupportsSelfReferencesInOneCompleteWrite(t *testing.T) {
	tables := catalogFixture()
	sc := tables["tasks"].Schema
	sc.AcceptReferences = true
	sc.Constraints[0].References.Table = "tasks"
	tables["tasks"].Rows[1].Values["project_id"] = int64(2)
	tables["tasks"].Rows[2] = Row{Values: map[string]any{"slug": "second", "active": true, "project_id": int64(2)}}
	if err := CheckCatalog(t.Context(), tables); err != nil {
		t.Fatal(err)
	}
	delete(tables["tasks"].Rows, 2)
	if err := CheckCatalog(t.Context(), tables); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("dangling self-reference: %v", err)
	}
}

func TestCatalogRequiresValidDeclarationsEvenWithoutRows(t *testing.T) {
	for _, mode := range []string{"missing_parent", "not_opted_in", "mixed_policy", "invalid_predicate", "missing_schema"} {
		t.Run(mode, func(t *testing.T) {
			tables := catalogFixture()
			delete(tables["tasks"].Rows, 1)
			switch mode {
			case "missing_parent":
				delete(tables, "projects")
			case "not_opted_in":
				tables["projects"].Schema.AcceptReferences = false
			case "mixed_policy":
				tables["projects"].Schema.RowAccess = schema.RowAccessOwn
			case "invalid_predicate":
				tables["tasks"].Schema.Constraints[1].Where = "active = 'not-a-boolean'"
			case "missing_schema":
				tables["projects"] = Table{}
			}
			if err := CheckCatalog(t.Context(), tables); err == nil {
				t.Fatal("invalid catalog accepted without rows")
			}
		})
	}
}

func TestCatalogOwnerDomainsAndCancellation(t *testing.T) {
	tables := catalogFixture()
	tables["projects"].Schema.RowAccess = schema.RowAccessOwn
	tables["tasks"].Schema.RowAccess = schema.RowAccessOwn
	alice := "alice"
	tables["tasks"].Rows[1] = Row{Owner: &alice, Values: map[string]any{"project_id": int64(1), "slug": "first", "active": true}}
	if err := CheckCatalog(t.Context(), tables); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("cross-domain reference: %v", err)
	}
	tables["projects"].Rows[1] = Row{Owner: &alice}
	if err := CheckCatalog(t.Context(), tables); err != nil {
		t.Fatal(err)
	}
	tables["tasks"].Rows[2] = Row{Values: map[string]any{"slug": "first", "active": true}}
	if err := CheckCatalog(t.Context(), tables); err != nil {
		t.Fatalf("unowned unique domain was collapsed: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := CheckCatalog(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("empty snapshot cancellation: %v", err)
	}
}

func TestWriteSelectionDoesNotCheckUnrelatedConstraints(t *testing.T) {
	tables := catalogFixture()
	tables["unrelated"] = Table{Schema: &schema.TableSchema{Name: "unrelated", Fields: []schema.Field{{Name: "slug", Type: schema.String}}, Constraints: []schema.Constraint{{Name: "other_slug", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}}, Rows: map[int64]Row{1: {Values: map[string]any{"slug": "same"}}, 2: {Values: map[string]any{"slug": "same"}}}}
	selected := SelectChecks(tables, "tasks", false)
	if err := CheckCatalog(t.Context(), selected); err != nil {
		t.Fatalf("unrelated constraints affected the write: %v", err)
	}
	if len(tables["unrelated"].Schema.Constraints) != 1 {
		t.Fatal("selection mutated source metadata")
	}
	if !selected["projects"].Schema.AcceptReferences {
		t.Fatal("selection lost target policy")
	}
	if err := CheckCatalog(t.Context(), tables); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("full catalog no longer checks unrelated constraints: %v", err)
	}
	selected["tasks"].Schema.Constraints[0].Name = "renamed"
	if tables["tasks"].Schema.Constraints[0].Name != "project_ref" {
		t.Fatal("selection shares its declaration slice")
	}
}

func TestParentDeleteSelectionChecksOnlyIncomingForeignKeys(t *testing.T) {
	tables := catalogFixture()
	delete(tables["projects"].Rows, 1)
	selected := SelectChecks(tables, "projects", true)
	cs := selected["tasks"].Schema.Constraints
	if len(cs) != 1 || cs[0].Name != "project_ref" {
		t.Fatalf("incoming selection: %+v", cs)
	}
	if err := CheckCatalog(t.Context(), selected); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("parent deletion ignored incoming references: %v", err)
	}
	if err := CheckCatalog(t.Context(), SelectChecks(tables, "projects", false)); err != nil {
		t.Fatalf("ordinary parent writes consulted incoming rows: %v", err)
	}
	if len(tables["tasks"].Schema.Constraints) != 2 {
		t.Fatal("selection removed original declarations")
	}
}
