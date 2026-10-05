package conformance

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseTableEngine interface {
	PlanMigration(context.Context, string, string, []schema.Change, store.Embedder, store.Incarnation, *store.RowScope, store.Incarnation) (*store.MigrationPlan, error)
	namespaceEngine
	CreateTable(context.Context, string, string, []schema.Field, store.TableOpts, [16]byte) (*schema.TableSchema, error)
	TableState(context.Context, string, string, []store.AuthBinding) (*schema.TableSchema, store.Incarnation, error)
	DescribeTable(context.Context, string, string, *store.RowScope, store.Incarnation) (*schema.TableSchema, int64, error)
	ListTables(context.Context, string, []store.AuthBinding) ([]string, error)
	DropTable(context.Context, string, string, store.Incarnation) error
	Migrate(context.Context, string, string, []schema.Change, store.Embedder, store.Incarnation) (*schema.TableSchema, error)
	ListMigrations(context.Context, string, string, store.Incarnation) ([]store.Migration, error)
}

func TestLakehouseTableBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			open := func() lakehouseTableEngine {
				var raw namespaceEngine
				var err error
				if backend == "lakehouse" {
					raw, err = lakehouse.Open(dir)
				} else {
					raw, err = store.Open(dir)
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { raw.Close() })
				eng, ok := raw.(lakehouseTableEngine)
				if !ok {
					t.Fatal("engine has no table lifecycle and schema registry")
				}
				return eng
			}
			eng := open()
			ctx := t.Context()
			ns := "project/team"
			if err := eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
				t.Fatal(err)
			}

			t.Run("enum_keeps_default", func(t *testing.T) {
				fields := []schema.Field{{Name: "tag", Type: schema.String, Enum: []string{"one", "two"}, Default: "one"}}
				if _, err := eng.CreateTable(ctx, ns, "enum_defaults", fields, store.TableOpts{}, [16]byte{}); err != nil {
					t.Fatal(err)
				}
				defer eng.DropTable(ctx, ns, "enum_defaults", store.Incarnation{})
				_, inc, err := eng.TableState(ctx, ns, "enum_defaults", nil)
				if err != nil {
					t.Fatal(err)
				}
				values := []string{"two"}
				changes := []schema.Change{{Op: schema.OpSetEnum, Name: "tag", Enum: &values}}
				if _, err := eng.PlanMigration(ctx, ns, "enum_defaults", changes, store.Embedder{}, inc, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("plan allowed an enum excluding the default: %v", err)
				}
				if _, err := eng.Migrate(ctx, ns, "enum_defaults", changes, store.Embedder{}, inc); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("migration allowed an enum excluding the default: %v", err)
				}
				unchanged, after, err := eng.TableState(ctx, ns, "enum_defaults", nil)
				if err != nil || after != inc || !reflect.DeepEqual(unchanged.Fields, fields) {
					t.Fatalf("invalid enum changed table state: %v %v %v", unchanged, after, err)
				}
			})
			t.Run("same_name_rename", func(t *testing.T) {
				fields := []schema.Field{{Name: "body", Type: schema.Text}}
				if _, err := eng.CreateTable(ctx, ns, "same_name", fields, store.TableOpts{}, [16]byte{}); err != nil {
					t.Fatal(err)
				}
				defer eng.DropTable(ctx, ns, "same_name", store.Incarnation{})
				_, inc, err := eng.TableState(ctx, ns, "same_name", nil)
				if err != nil {
					t.Fatal(err)
				}
				changes := []schema.Change{{Op: schema.OpRenameField, From: "body", To: "body"}}
				plan, err := eng.PlanMigration(ctx, ns, "same_name", changes, store.Embedder{}, inc, nil, store.Incarnation{})
				if err != nil || !reflect.DeepEqual(plan.Table.Fields, fields) {
					t.Fatalf("same-name rename plan: %v %v", plan, err)
				}
				next, err := eng.Migrate(ctx, ns, "same_name", changes, store.Embedder{}, inc)
				if err != nil || int64(next.Version) != inc.Version+1 || !reflect.DeepEqual(next.Fields, fields) {
					t.Fatalf("same-name rename: %v %v", next, err)
				}
			})
			t.Run("plan_incarnation_token", func(t *testing.T) {
				if _, err := eng.CreateTable(ctx, ns, "plan_token", []schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
					t.Fatal(err)
				}
				defer eng.DropTable(ctx, ns, "plan_token", store.Incarnation{})
				_, inc, err := eng.TableState(ctx, ns, "plan_token", nil)
				if err != nil {
					t.Fatal(err)
				}
				changes := []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "title", Type: schema.String}}}
				plan, err := eng.PlanMigration(ctx, ns, "plan_token", changes, store.Embedder{}, inc, nil, store.Incarnation{})
				if err != nil || plan.ExpectedIncarnation != store.EncodeIncarnation(inc) {
					t.Fatalf("plan incarnation token: %v %v", plan, err)
				}
				_, after, err := eng.TableState(ctx, ns, "plan_token", nil)
				if err != nil || after != inc {
					t.Fatalf("plan changed incarnation: %v %v", after, err)
				}
			})

			kept, excluded := []string{"one"}, []string{"two"}
			objectShape := "object"
			addTag := schema.Change{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}, Default: "one"}
			addPayload := schema.Change{Op: schema.OpAddField, Field: &schema.Field{Name: "payload", Type: schema.JSON}, Default: []any{"one"}}
			for _, tc := range []struct {
				name    string
				changes []schema.Change
				valid   bool
			}{
				{"enum_backfill", []schema.Change{addTag, {Op: schema.OpSetEnum, Name: "tag", Enum: &excluded}}, false},
				{"enum_renamed_backfill", []schema.Change{addTag, {Op: schema.OpRenameField, From: "tag", To: "label"}, {Op: schema.OpSetEnum, Name: "label", Enum: &excluded}}, false},
				{"shape_backfill", []schema.Change{addPayload, {Op: schema.OpSetShape, Name: "payload", Shape: &objectShape}}, false},
				{"shape_renamed_backfill", []schema.Change{addPayload, {Op: schema.OpRenameField, From: "payload", To: "document"}, {Op: schema.OpSetShape, Name: "document", Shape: &objectShape}}, false},
				{"enum_backfill_allowed", []schema.Change{addTag, {Op: schema.OpSetEnum, Name: "tag", Enum: &kept}}, true},
				{"shape_backfill_allowed", []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "payload", Type: schema.JSON}, Default: map[string]any{"one": true}}, {Op: schema.OpSetShape, Name: "payload", Shape: &objectShape}}, true},
				{"enum_drop_readd", []schema.Change{addTag, {Op: schema.OpDropField, Name: "tag"}, {Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}}, {Op: schema.OpSetEnum, Name: "tag", Enum: &excluded}}, true},
				{"shape_drop_readd", []schema.Change{addPayload, {Op: schema.OpDropField, Name: "payload"}, {Op: schema.OpAddField, Field: &schema.Field{Name: "payload", Type: schema.JSON}}, {Op: schema.OpSetShape, Name: "payload", Shape: &objectShape}}, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					fields := []schema.Field{{Name: "body", Type: schema.Text}}
					if _, err := eng.CreateTable(ctx, ns, "backfill_checks", fields, store.TableOpts{}, [16]byte{}); err != nil {
						t.Fatal(err)
					}
					defer eng.DropTable(ctx, ns, "backfill_checks", store.Incarnation{})
					_, inc, err := eng.TableState(ctx, ns, "backfill_checks", nil)
					if err != nil {
						t.Fatal(err)
					}
					plan, planErr := eng.PlanMigration(ctx, ns, "backfill_checks", tc.changes, store.Embedder{}, inc, nil, store.Incarnation{})
					if !tc.valid {
						if !errors.Is(planErr, store.ErrInvalid) {
							t.Fatalf("plan allowed a conflicting backfill: %v %v", plan, planErr)
						}
						if _, err := eng.Migrate(ctx, ns, "backfill_checks", tc.changes, store.Embedder{}, inc); !errors.Is(err, store.ErrInvalid) {
							t.Fatalf("migration allowed a conflicting backfill: %v", err)
						}
						unchanged, after, err := eng.TableState(ctx, ns, "backfill_checks", nil)
						if err != nil || after != inc || !reflect.DeepEqual(unchanged.Fields, fields) {
							t.Fatalf("invalid backfill changed table state: %v %v %v", unchanged, after, err)
						}
						return
					}
					if planErr != nil {
						t.Fatalf("valid backfill plan: %v", planErr)
					}
					next, err := eng.Migrate(ctx, ns, "backfill_checks", tc.changes, store.Embedder{}, inc)
					if err != nil || !reflect.DeepEqual(next, plan.Table) {
						t.Fatalf("valid backfill migration: %v %v", next, err)
					}
				})
			}

			enumValues := []string{"one"}
			indexFields := []schema.Field{{Name: "tag", Type: schema.String, Enum: enumValues}, {Name: "body", Type: schema.Text, Fulltext: true, Vectorize: true}}
			if _, err := eng.CreateTable(ctx, ns, "indexed_notes", indexFields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			_, indexed, err := eng.TableState(ctx, ns, "indexed_notes", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				change         schema.Change
				rebuild, clear bool
			}{
				{schema.Change{Op: schema.OpAddField, Field: &schema.Field{Name: "extra", Type: schema.Text, Fulltext: true}}, true, false},
				{schema.Change{Op: schema.OpRenameField, From: "body", To: "content"}, true, false},
				{schema.Change{Op: schema.OpDropField, Name: "body"}, true, true},
			} {
				plan, err := eng.PlanMigration(ctx, ns, "indexed_notes", []schema.Change{tc.change}, store.Embedder{}, indexed, nil, store.Incarnation{})
				if err != nil || plan.RebuildFulltext != tc.rebuild || plan.ClearsEmbeddings != tc.clear {
					t.Fatalf("index plan %s: %v %v", tc.change.Op, plan, err)
				}
			}
			emptyEnum := []string{}
			removed, err := eng.Migrate(ctx, ns, "indexed_notes", []schema.Change{{Op: schema.OpSetEnum, Name: "tag", Enum: &emptyEnum}}, store.Embedder{}, indexed)
			if err != nil || removed.Fields[0].Enum != nil {
				t.Fatalf("remove enum: %v %v", removed, err)
			}
			if err := eng.DropTable(ctx, ns, "indexed_notes", store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{{Name: "title", Type: schema.String, Fulltext: true}, {Name: "amount", Type: schema.Number}, {Name: "active", Type: schema.Boolean}, {Name: "stamp", Type: schema.Timestamp}, {Name: "payload", Type: schema.JSON}, {Name: "vector", Type: schema.Vector, Dim: 3}}
			sc, err := eng.CreateTable(ctx, ns, "notes", fields, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{})
			if err != nil || sc.Version != 1 || !sc.HasOwner {
				t.Fatalf("create: %v %v", sc, err)
			}
			_, old, err := eng.TableState(ctx, ns, "notes", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"../escape", "id", "bad-name", "Upper"} {
				if _, err := eng.CreateTable(ctx, ns, name, fields, store.TableOpts{}, [16]byte{}); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("invalid table %q: %v", name, err)
				}
			}
			if _, err := eng.CreateTable(ctx, ns, "notes", fields, store.TableOpts{}, [16]byte{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("duplicate: %v", err)
			}
			if _, err := eng.CreateTable(ctx, ns, "bad", []schema.Field{{Name: "owner", Type: schema.String}}, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("owner collision: %v", err)
			}
			changes := []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "body", Type: schema.Text}}, {Op: schema.OpRenameField, From: "title", To: "heading"}, {Op: schema.OpDropField, Name: "active"}}
			next, err := eng.Migrate(ctx, ns, "notes", changes, store.Embedder{}, old)
			if err != nil || next.Version != 2 || next.Fields[0].Name != "heading" {
				t.Fatalf("evolve: %v %v", next, err)
			}
			if _, err := eng.Migrate(ctx, ns, "notes", changes, store.Embedder{}, old); !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalid) {
				var conflict *store.VersionConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("stale migration: %v", err)
				}
			}
			_, current, err := eng.TableState(ctx, ns, "notes", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Migrate(ctx, ns, "notes", []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "temporary", Type: schema.Text}}, {Op: schema.OpRenameField, From: "missing", To: "nope"}}, store.Embedder{}, current); err == nil {
				t.Fatal("invalid multi-step migration accepted")
			}
			described, count, err := eng.DescribeTable(ctx, ns, "notes", nil, current)
			if err != nil || count != 0 || !reflect.DeepEqual(described, next) {
				t.Fatalf("describe/failed migration: %v %d %v", described, count, err)
			}
			history, err := eng.ListMigrations(ctx, ns, "notes", current)
			if err != nil || len(history) != 1 || history[0].FromVersion != 1 || history[0].ToVersion != 2 {
				t.Fatalf("history: %v %v", history, err)
			}
			if err := eng.Close(); err != nil {
				t.Fatal(err)
			}
			eng = open()
			reopened, again, err := eng.TableState(ctx, ns, "notes", nil)
			if err != nil || again != current || !reflect.DeepEqual(reopened, next) {
				t.Fatalf("reopen: %v %v %v", reopened, again, err)
			}
			names, err := eng.ListTables(ctx, ns, nil)
			if err != nil || !reflect.DeepEqual(names, []string{"notes"}) {
				t.Fatalf("list: %v %v", names, err)
			}
			if err := eng.DropTable(ctx, ns, "notes", current); err != nil {
				t.Fatal(err)
			}
			if _, _, err := eng.TableState(ctx, ns, "notes", nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("dropped: %v", err)
			}
			if _, err := eng.CreateTable(ctx, ns, "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			_, successor, err := eng.TableState(ctx, ns, "notes", nil)
			if err != nil || successor.DropGen <= current.DropGen {
				t.Fatalf("successor: %v %v", successor, err)
			}
			if err := eng.DropTable(ctx, ns, "notes", current); err == nil {
				t.Fatal("predecessor guard dropped successor")
			}
			history, err = eng.ListMigrations(ctx, ns, "notes", successor)
			if err != nil || len(history) != 0 {
				t.Fatalf("successor history: %v %v", history, err)
			}
		})
	}
}
