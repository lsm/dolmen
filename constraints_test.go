package dolmen

import (
	"context"
	"errors"
	"reflect"
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
	if !reflect.DeepEqual(sc.Constraints, declared) {
		t.Fatalf("created schema: %+v", sc.Constraints)
	}
	tasks, _, err := st.DescribeTable(ctx, "app", "tasks")
	if err != nil || !reflect.DeepEqual(tasks.Constraints, declared) {
		t.Fatalf("described constraints: %+v %v", tasks.Constraints, err)
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

func TestCreateTableOptionsKeepNormalizationAndValidation(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	sc, err := st.CreateTableWithOptions(ctx, " App ", " Notes ", []Field{{Name: "slug", Type: String}}, CreateTableOptions{RowAccess: "own"})
	if err != nil || sc.Namespace != "app" || sc.Name != "notes" || sc.RowAccess != "own" || sc.Relation != nil || len(sc.Constraints) != 0 || sc.AcceptReferences {
		t.Fatalf("normalized schema: %+v %v", sc, err)
	}
	if _, err := st.CreateTableWithOptions(ctx, "app", "bad-name", []Field{{Name: "body", Type: Text}}, CreateTableOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("malformed name: %v", err)
	}
	if _, err := st.CreateTableWithOptions(ctx, "app", "bad_policy", []Field{{Name: "body", Type: Text}}, CreateTableOptions{RowAccess: "invalid"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid row access: %v", err)
	}
	for _, c := range []Constraint{nil, (*UniqueConstraint)(nil), strayConstraint{}} {
		if _, err := st.CreateTableWithOptions(ctx, "app", "bad_constraint", []Field{{Name: "body", Type: Text}}, CreateTableOptions{Constraints: []Constraint{c}}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid constraint value: %v", err)
		}
	}
}
