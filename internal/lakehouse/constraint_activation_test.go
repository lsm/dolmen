package lakehouse

import (
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestLakehouseConstraintActivationAndDryRun(t *testing.T) {
	for _, declaration := range []bool{true, false} {
		t.Run(map[bool]string{true: "declaration", false: "migration"}[declaration], func(t *testing.T) {
			s := openStore(t, t.TempDir(), WithSharedFilter(true))
			ctx := t.Context()
			if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			unique := schema.Constraint{Name: "active_slug", Kind: schema.ConstraintUnique, Fields: []string{"slug"}, Where: "done = FALSE"}
			opts := store.TableOpts{}
			if declaration {
				opts.Constraints = []schema.Constraint{unique}
			}
			if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "slug", Type: schema.String}, {Name: "done", Type: schema.Boolean}}, opts, [16]byte{}); err != nil {
				t.Fatalf("create constrained table: %v", err)
			}
			insert := func(done bool) error {
				_, err := s.Insert(ctx, "app", "notes", []map[string]any{{"slug": "same", "done": done}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
				return err
			}
			if err := insert(false); err != nil {
				t.Fatal(err)
			}
			add := []schema.Change{{Op: schema.OpAddConstraint, Constraint: &unique}}
			if !declaration {
				if err := insert(false); err != nil {
					t.Fatal(err)
				}
				if _, err := s.PlanMigration(ctx, "app", "notes", add, store.Embedder{}, store.Incarnation{}, nil, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "active_slug") {
					t.Fatalf("dry run must report duplicate rows: %v", err)
				}
				if sc, _, err := s.DescribeTable(ctx, "app", "notes", nil, store.Incarnation{}); err != nil || sc.Version != 1 || len(sc.Constraints) != 0 {
					t.Fatalf("refused dry run changed schema: %+v %v", sc, err)
				}
				if format := lakeFormat(t, s); format != catalogFormat {
					t.Fatalf("refused dry run raised reader format: %d", format)
				}
				if _, err := s.Delete(ctx, "app", "notes", "id = 2", nil, store.DeleteOpts{}, nil, store.Incarnation{}); err != nil {
					t.Fatal(err)
				}
				plan, err := s.PlanMigration(ctx, "app", "notes", add, store.Embedder{}, store.Incarnation{}, nil, store.Incarnation{})
				if err != nil || !plan.DryRun || len(plan.Table.Constraints) != 1 {
					t.Fatalf("feasible dry run: %+v %v", plan, err)
				}
				if sc, _, err := s.DescribeTable(ctx, "app", "notes", nil, store.Incarnation{}); err != nil || len(sc.Constraints) != 0 || sc.Version != 1 {
					t.Fatalf("feasible dry run changed schema: %+v %v", sc, err)
				}
				if err := insert(false); err != nil {
					t.Fatalf("dry run enforced a constraint: %v", err)
				}
				if _, err := s.Migrate(ctx, "app", "notes", add, store.Embedder{}, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) {
					t.Fatalf("execution did not recheck rows: %v", err)
				}
				if _, err := s.Delete(ctx, "app", "notes", "id > 1", nil, store.DeleteOpts{}, nil, store.Incarnation{}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Migrate(ctx, "app", "notes", add, store.Embedder{}, store.Incarnation{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := insert(false); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("duplicate must conflict: %v", err)
			}
			if err := insert(true); err != nil {
				t.Fatalf("row outside predicate: %v", err)
			}
			if err := s.evict("app"); err != nil {
				t.Fatal(err)
			}
			if err := insert(false); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("reopen lost constraint: %v", err)
			}
			var format int
			if err := s.withNamespace(ctx, "app", func(n *namespace) error {
				return n.db.QueryRowContext(ctx, `SELECT format FROM _dolmen_lakehouse_meta WHERE singleton=1`).Scan(&format)
			}); err != nil || format != 4 {
				t.Fatalf("reader guard after reopen: format=%d err=%v", format, err)
			}
		})
	}
}

func TestLakehouseReaderGuardFailureDoesNotPublishConstraints(t *testing.T) {
	for _, migration := range []bool{false, true} {
		t.Run(map[bool]string{false: "declaration", true: "migration"}[migration], func(t *testing.T) {
			s := openStore(t, t.TempDir(), WithSharedFilter(true))
			ctx := t.Context()
			if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{{Name: "slug", Type: schema.String}}
			if migration {
				if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.withNamespace(ctx, "app", func(n *namespace) error {
				_, err := n.db.ExecContext(ctx, `CREATE TRIGGER refuse_constraint_format BEFORE UPDATE OF format ON _dolmen_lakehouse_meta WHEN NEW.format > OLD.format BEGIN SELECT RAISE(FAIL,'synthetic format refusal'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			unique := schema.Constraint{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}
			var err error
			if migration {
				_, err = s.Migrate(ctx, "app", "notes", []schema.Change{{Op: schema.OpAddConstraint, Constraint: &unique}}, store.Embedder{}, store.Incarnation{})
			} else {
				_, err = s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{Constraints: []schema.Constraint{unique}}, [16]byte{})
			}
			if err == nil {
				t.Fatal("reader guard failure was ignored")
			}
			sc, _, err := s.DescribeTable(ctx, "app", "notes", nil, store.Incarnation{})
			if migration {
				if err != nil || sc.Version != 1 || len(sc.Constraints) != 0 {
					t.Fatalf("published constraints without reader guard: %+v %v", sc, err)
				}
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("created constrained table without reader guard: %+v %v", sc, err)
			}
		})
	}
}
