package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type migrationEngine interface {
	insertEngine
	PlanMigration(context.Context, string, string, []schema.Change, store.Embedder, store.Incarnation, *store.RowScope, store.Incarnation) (*store.MigrationPlan, error)
	Migrate(context.Context, string, string, []schema.Change, store.Embedder, store.Incarnation) (*schema.TableSchema, error)
	ListMigrations(context.Context, string, string, store.Incarnation) ([]store.Migration, error)
}

func boolPtr(v bool) *bool { return &v }

func enumPtr(v []string) *[]string { return &v }

func migrationTestEngine(t *testing.T, backend string) migrationEngine {
	t.Helper()
	if backend == "postgres" {
		return postgresNamespaceEngine(t).(migrationEngine)
	}
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			eng := migrationTestEngine(t, backend)
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{{Name: "title", Fulltext: true}, {Name: "state"}, {Name: "score", Type: schema.Number}}
			if _, err := eng.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			emb := store.Embedder{Identity: "test", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
				vectors := make([][]float32, len(texts))
				for i := range vectors {
					vectors[i] = []float32{1, 2, float32(len(texts[i]))}
				}
				return vectors, nil
			}}
			if _, err := eng.Insert(ctx, "app", "notes", []map[string]any{
				{"title": "alpha", "state": "open", "score": 1},
				{"title": "beta", "state": "done", "score": 2},
			}, store.WriteOpts{}, emb, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}

			plan, err := eng.PlanMigration(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpAddField, Field: &schema.Field{Name: "owner"}, Default: "unassigned"},
			}, emb, store.Incarnation{Version: 1}, nil, store.Incarnation{})
			if err != nil {
				t.Fatal(err)
			}
			if !plan.DryRun || plan.FromVersion != 1 || plan.ToVersion != 2 || plan.BackfillRows != 2 {
				t.Fatalf("plan: %+v", plan)
			}
			if len(plan.Operations) != 1 || plan.Operations[0] != `add_field owner (string, default "unassigned")` {
				t.Fatalf("plan operations: %+v", plan.Operations)
			}
			if got, err := eng.ListMigrations(ctx, "app", "notes", store.Incarnation{}); err != nil || len(got) != 0 {
				t.Fatalf("dry run recorded history: %+v %v", got, err)
			}

			sc, err := eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpAddField, Field: &schema.Field{Name: "owner"}, Default: "unassigned"},
			}, emb, store.Incarnation{Version: 1})
			if err != nil {
				t.Fatal(err)
			}
			if sc.Version != 2 || sc.Field("owner") == nil {
				t.Fatalf("schema after add: %+v", sc)
			}
			rows, err := eng.GetRows(ctx, "app", "notes", []int64{1, 2}, nil, store.Incarnation{})
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows.Rows {
				if row["owner"] != "unassigned" {
					t.Fatalf("backfill: %+v", row)
				}
			}

			if _, err := eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpAddField, Field: &schema.Field{Name: "late"}},
			}, emb, store.Incarnation{Version: 1}); err == nil {
				t.Fatal("stale expected_version accepted")
			} else {
				var conflict *store.VersionConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("version conflict: %v", err)
				}
				if conflict.CurrentVersion != 2 || conflict.ExpectedVersion != 1 {
					t.Fatalf("conflict detail: %+v", conflict)
				}
			}

			if _, err := eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpDropField, Name: "score"},
			}, emb, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("destructive change without expected_version: %v", err)
			}

			if _, err := eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpSetEnum, Name: "state", Enum: enumPtr([]string{"open"})},
			}, emb, store.Incarnation{Version: 2}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("enum excluding stored values: %v", err)
			}

			sc, err = eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpRenameField, From: "title", To: "headline"},
				{Op: schema.OpSetEnum, Name: "state", Enum: enumPtr([]string{"open", "done"})},
			}, emb, store.Incarnation{Version: 2})
			if err != nil {
				t.Fatal(err)
			}
			if sc.Version != 3 || sc.Field("title") != nil || sc.Field("headline") == nil {
				t.Fatalf("schema after rename: %+v", sc)
			}
			if got := sc.Field("state").Enum; len(got) != 2 || got[0] != "open" || got[1] != "done" {
				t.Fatalf("enum: %+v", got)
			}
			rows, err = eng.GetRows(ctx, "app", "notes", []int64{1}, nil, store.Incarnation{})
			if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["headline"] != "alpha" {
				t.Fatalf("rename preserved data: %+v %v", rows.Rows, err)
			}

			sc, err = eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpSetVectorize, Name: "headline", Value: boolPtr(true)},
			}, emb, store.Incarnation{Version: 3})
			if err != nil {
				t.Fatal(err)
			}
			if sc.EmbedSpace != "test" || sc.EmbedDim != 3 {
				t.Fatalf("embedding space: %+v", sc)
			}
			if vf := sc.VectorizeField(); vf == nil || vf.Name != "headline" {
				t.Fatalf("vectorize field: %+v", vf)
			}

			sc, err = eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpDropField, Name: "score"},
			}, emb, store.Incarnation{Version: 4})
			if err != nil {
				t.Fatal(err)
			}
			if sc.Version != 5 || sc.Field("score") != nil {
				t.Fatalf("schema after drop: %+v", sc)
			}

			history, err := eng.ListMigrations(ctx, "app", "notes", store.Incarnation{})
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 4 {
				t.Fatalf("history length: %+v", history)
			}
			if history[0].FromVersion != 4 || history[0].ToVersion != 5 {
				t.Fatalf("history newest first: %+v", history[0])
			}
			if history[3].FromVersion != 1 || history[3].ToVersion != 2 {
				t.Fatalf("history oldest last: %+v", history[3])
			}
			for i := 1; i < len(history); i++ {
				if history[i].ID >= history[i-1].ID {
					t.Fatalf("history ids not descending: %+v", history)
				}
			}
			if history[0].At == "" {
				t.Fatalf("history timestamp: %+v", history[0])
			}

			if _, err := eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpSetFulltext, Name: "state", Value: boolPtr(true)},
				{Op: schema.OpSetVectorize, Name: "state", Value: boolPtr(true)},
			}, emb, store.Incarnation{Version: 5}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("second vectorized field: %v", err)
			}
			state, _, err := eng.TableState(ctx, "app", "notes", nil)
			if err != nil {
				t.Fatal(err)
			}
			if state.Version != 5 {
				t.Fatalf("failed migration bumped version: %+v", state)
			}
		})
	}
}

func TestMigrationDropTableClearsHistoryBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			eng := migrationTestEngine(t, backend)
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			emb := store.Embedder{}
			create := func() {
				if _, err := eng.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "title"}}, store.TableOpts{}, [16]byte{}); err != nil {
					t.Fatal(err)
				}
			}
			create()
			if _, err := eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpAddField, Field: &schema.Field{Name: "owner"}},
			}, emb, store.Incarnation{Version: 1}); err != nil {
				t.Fatal(err)
			}
			if got, err := eng.ListMigrations(ctx, "app", "notes", store.Incarnation{}); err != nil || len(got) != 1 {
				t.Fatalf("history before drop: %+v %v", got, err)
			}
			if err := eng.DropTable(ctx, "app", "notes", store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			create()
			got, err := eng.ListMigrations(ctx, "app", "notes", store.Incarnation{})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("history survived drop: %+v", got)
			}
		})
	}
}

func TestMigrationEmptyTableVectorizeBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			eng := migrationTestEngine(t, backend)
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "code"}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			emb := store.Embedder{Identity: "test", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
				out := make([][]float32, len(texts))
				for i := range out {
					out[i] = []float32{1, 2, 3}
				}
				return out, nil
			}}
			sc, err := eng.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpAddField, Field: &schema.Field{Name: "summary", Vectorize: true}, Default: "placeholder"},
			}, emb, store.Incarnation{Version: 1})
			if err != nil {
				t.Fatal(err)
			}
			if sc.EmbedDim != 0 {
				t.Fatalf("no rows were embedded but embed_dim is %d", sc.EmbedDim)
			}
			if sc.EmbedSpace != "test" {
				t.Fatalf("embed_space: %q", sc.EmbedSpace)
			}
		})
	}
}

func TestMigrationRejectsNegativeExpectedVersionBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			eng := migrationTestEngine(t, backend)
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			changes := []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "extra"}}}
			_, err := eng.Migrate(ctx, "app", "notes", changes, store.Embedder{}, store.Incarnation{Version: -1})
			if !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("negative expected_version: %v", err)
			}
			var conflict *store.VersionConflictError
			if errors.As(err, &conflict) {
				t.Fatalf("negative expected_version reported as a conflict: %v", err)
			}
			if _, err := eng.PlanMigration(ctx, "app", "notes", changes, store.Embedder{}, store.Incarnation{Version: -1}, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("plan with negative expected_version: %v", err)
			}
		})
	}
}
