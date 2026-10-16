package lakehouse

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestDormantFileConstraintMigrationsValidateStoredReferences(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []schema.Change
		want    string
	}{
		{"too small", []schema.Change{{Op: schema.OpSetFileConstraints, Name: "doc", MaxBytes: filePtr(int64(4))}}, "1 rows hold files that break them (ids 1)"},
		{"excluded type", []schema.Change{{Op: schema.OpSetFileConstraints, Name: "doc", MediaTypes: filePtr([]string{"image/*"})}}, "ids 1"},
		{"renamed", []schema.Change{{Op: schema.OpRenameField, From: "doc", To: "scan"}, {Op: schema.OpSetFileConstraints, Name: "scan", MaxBytes: filePtr(int64(4))}}, "field \"scan\""},
		{"intermediate limit", []schema.Change{{Op: schema.OpSetFileConstraints, Name: "doc", MaxBytes: filePtr(int64(4))}, {Op: schema.OpSetFileConstraints, Name: "doc", MaxBytes: filePtr(int64(100))}}, "ids 1"},
		{"no arguments", []schema.Change{{Op: schema.OpSetFileConstraints, Name: "doc"}}, "needs max_bytes"},
		{"wrong operation", []schema.Change{{Op: schema.OpDropField, Name: "doc", MaxBytes: filePtr(int64(0))}}, "only allowed on set_file_constraints"},
		{"non file", []schema.Change{{Op: schema.OpSetFileConstraints, Name: "title", MaxBytes: filePtr(int64(4))}}, "only allowed on file fields"},
		{"negative", []schema.Change{{Op: schema.OpSetFileConstraints, Name: "doc", MaxBytes: filePtr(int64(-1))}}, "max_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dormantFileStore(t)
			ctx := store.WithFileSource(t.Context(), &testFiles{})
			if _, err := s.Insert(ctx, "ns", "docs", []map[string]any{{"doc": map[string]any{"part": "f"}}, {}}, store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			before, inc, err := s.TableState(ctx, "ns", "docs", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.PlanMigration(ctx, "ns", "docs", tc.changes, store.Embedder{}, inc, nil, inc); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("plan: %v; want %q", err, tc.want)
			}
			if _, err := s.Migrate(ctx, "ns", "docs", tc.changes, store.Embedder{}, inc); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("migrate: %v; want %q", err, tc.want)
			}
			after, _, err := s.TableState(ctx, "ns", "docs", nil)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refused migration changed metadata: %+v %v", after, err)
			}
		})
	}
}

func TestDormantFileConstraintsAreSchemaOnlyAndApplyToTheNextWrite(t *testing.T) {
	s := dormantFileStore(t)
	ctx := store.WithFileSource(t.Context(), &testFiles{})
	if _, err := s.Insert(ctx, "ns", "docs", []map[string]any{{"doc": map[string]any{"part": "f"}}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, snapshots := scannedRows(t, s, "ns", "docs")
	changes := []schema.Change{{Op: schema.OpSetFileConstraints, Name: "doc", MaxBytes: filePtr(int64(5)), MediaTypes: filePtr([]string{})}}
	plan, err := s.PlanMigration(ctx, "ns", "docs", changes, store.Embedder{}, store.Incarnation{}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.BackfillRows != 0 || plan.EmbedRows != 0 {
		t.Fatalf("not schema-only: %+v", plan)
	}
	next, err := s.Migrate(ctx, "ns", "docs", changes, store.Embedder{}, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if next.Version != 2 || next.Field("doc").MaxBytes != 5 || next.Field("doc").MediaTypes != nil {
		t.Fatalf("constraints %+v", next)
	}
	if _, got := scannedRows(t, s, "ns", "docs"); got != snapshots {
		t.Fatalf("constraints rewrote data: %d snapshots -> %d", snapshots, got)
	}
	if _, err := s.Insert(ctx, "ns", "docs", []map[string]any{{"doc": map[string]any{"part": "f", "media_type": "image/png"}}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("cleared media constraint: %v", err)
	}
	if _, err := s.Insert(ctx, "ns", "docs", []map[string]any{{"doc": map[string]any{"part": "large"}}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("new size constraint: %v", err)
	}
	history, err := s.ListMigrations(ctx, "ns", "docs", store.Incarnation{})
	if err != nil || len(history) != 1 {
		t.Fatalf("history %+v %v", history, err)
	}
}

func TestDormantFileConstraintsHideViolatingRowsFromAScopedPlan(t *testing.T) {
	s := dormantFileStore(t)
	ctx := store.WithFileSource(t.Context(), &testFiles{})
	if _, err := s.Insert(ctx, "ns", "docs", []map[string]any{{"doc": map[string]any{"part": "f"}}}, store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.PlanMigration(ctx, "ns", "docs", []schema.Change{{Op: schema.OpSetFileConstraints, Name: "doc", MaxBytes: filePtr(int64(4))}}, store.Embedder{}, store.Incarnation{}, &store.RowScope{Owner: "bob"}, store.Incarnation{})
	if !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "Which rows, and how many") || strings.Contains(err.Error(), "ids 1") || strings.Contains(err.Error(), "1 rows") {
		t.Fatalf("scoped refusal: %v", err)
	}
}

func filePtr[T any](v T) *T { return &v }

func TestDormantFileEnumRefusalExplainsFileFields(t *testing.T) {
	s := dormantFileStore(t)
	_, err := s.PlanMigration(t.Context(), "ns", "docs", []schema.Change{{Op: schema.OpSetEnum, Name: "doc", Enum: filePtr([]string{"a"})}}, store.Embedder{}, store.Incarnation{}, nil, store.Incarnation{})
	if !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "not allowed on file fields") {
		t.Fatalf("file enum refusal: %v", err)
	}
}
