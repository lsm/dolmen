package lakehouse

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestLakehouseConstraintsCheckCommittedRowsPlusTheStagedWrite(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "projects", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "tasks", []schema.Field{{Name: "project_id", Type: schema.Number}, {Name: "serial", Type: schema.Number}, {Name: "active", Type: schema.Boolean}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "projects", []map[string]any{{"slug": "p"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "tasks", []map[string]any{{"project_id": int64(1), "serial": int64(math.MaxInt64), "active": true}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	declared := map[string]func(*schema.TableSchema){
		"projects": func(sc *schema.TableSchema) { sc.AcceptReferences = true },
		"tasks": func(sc *schema.TableSchema) {
			sc.Constraints = []schema.Constraint{
				{Name: "project_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: "projects", Field: "id"}},
				{Name: "active_serial", Kind: schema.ConstraintUnique, Fields: []string{"serial"}, Where: "active = TRUE"},
			}
		},
	}
	err := s.withNamespace(ctx, "app", func(n *namespace) error {
		for name, declare := range declared {
			state, err := loadTable(ctx, n, "app", name)
			if err != nil {
				return err
			}
			declare(state.schema)
			if err := s.publishSchema(ctx, n, state); err != nil {
				return err
			}
		}
		tasks, err := loadTableDeclared(ctx, n, "app", "tasks")
		if err != nil {
			return err
		}
		projects, err := loadTableDeclared(ctx, n, "app", "projects")
		if err != nil {
			return err
		}
		row := func(id int64, project any, serial int64, active bool) map[string]any {
			return map[string]any{"id": id, "project_id": project, "serial": canonicalNumber(serial), "active": active}
		}
		for label, tc := range map[string]struct {
			staged stagedRows
			want   string
		}{
			"distinct int64 keys next to the boundary": {stagedRows{rows: []map[string]any{row(2, "1", math.MaxInt64-1, true)}}, ""},
			"a duplicate of a committed key":           {stagedRows{rows: []map[string]any{row(2, "1", math.MaxInt64, true)}}, "active_serial"},
			"a duplicate outside the predicate":        {stagedRows{rows: []map[string]any{row(2, "1", math.MaxInt64, false)}}, ""},
			"an update that frees the key":             {stagedRows{rows: []map[string]any{row(1, "1", 5, true), row(2, "1", math.MaxInt64, true)}}, ""},
			"an orphan":                                {stagedRows{rows: []map[string]any{row(2, "9", 7, true)}}, "project_ref"},
			"a null reference":                         {stagedRows{rows: []map[string]any{row(2, nil, 7, true)}}, ""},
		} {
			err := s.checkConstraints(ctx, n, tasks, tc.staged, false)
			if tc.want == "" && err != nil || tc.want != "" && (!errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("%s: %v, want %q", label, err, tc.want)
			}
		}
		if err := s.checkConstraints(ctx, n, projects, stagedRows{deletes: []int64{1}}, true); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "project_ref") {
			t.Errorf("deleting a referenced parent: %v", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.checkConstraints(canceled, n, projects, stagedRows{}, false); err != nil {
			t.Errorf("an ordinary write to a table without constraints must not read the snapshot: %v", err)
		}
		if err := s.checkConstraints(canceled, n, projects, stagedRows{deletes: []int64{1}}, true); err == nil {
			t.Error("a delete from a table accepting references must read the snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
