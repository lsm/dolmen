package dolmen

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type strayConstraint struct{ UniqueConstraint }

func TestTheLibraryDeclaresAndEnforcesConstraints(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTableWithOptions(ctx, "app", "projects", []Field{{Name: "slug", Type: String}}, CreateTableOptions{AcceptReferences: true}); err != nil {
		t.Fatal(err)
	}
	declared := []Constraint{
		ForeignKeyConstraint{Name: "project_ref", Field: "project_id", ReferencesTable: "projects"},
		UniqueConstraint{Name: "open_slug", Fields: []string{"slug"}, Where: "done = FALSE"},
	}
	sc, err := st.CreateTableWithOptions(ctx, "app", "tasks", []Field{{Name: "project_id", Type: Number}, {Name: "slug", Type: String}, {Name: "done", Type: Boolean}}, CreateTableOptions{Constraints: declared})
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Constraints) != 2 || sc.Constraints[0] != declared[0] {
		t.Fatalf("created schema: %+v", sc.Constraints)
	}
	described, _, err := st.DescribeTable(ctx, "app", "projects")
	if err != nil || !described.AcceptReferences {
		t.Fatalf("describe lost accept_references: %+v %v", described, err)
	}
	if _, err := st.Insert(ctx, "app", "projects", []map[string]any{{"slug": "p"}}, InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "app", "tasks", []map[string]any{{"project_id": 1, "slug": "a", "done": false}}, InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, rec := range map[string]map[string]any{
		"open_slug":   {"project_id": 1, "slug": "a", "done": false},
		"project_ref": {"project_id": 7, "slug": "b", "done": false},
	} {
		if _, err := st.Insert(ctx, "app", "tasks", []map[string]any{rec}, InsertOptions{}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: %v, want ErrConflict naming it", name, err)
		}
	}
	if _, err := st.CreateTableWithOptions(ctx, "app", "notes", []Field{{Name: "slug", Type: String}}, CreateTableOptions{Constraints: []Constraint{strayConstraint{}}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a Constraint that is not one of the library's types: %v", err)
	}
}
