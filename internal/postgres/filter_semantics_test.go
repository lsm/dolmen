package postgres

import (
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresACrossClassComparisonStillPropagatesNull(t *testing.T) {
	cfg := testConfig(t)
	cfg.SharedFilter = true
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}, {Name: "tag", Type: schema.Text}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	records := []map[string]any{{"tag": "only"}}
	if _, err := s.Insert(ctx, "app", "notes", records, store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		filter string
		why    string
	}{
		{"body > 1", "text sorts after a number, but a null body compares to null, not to true"},
		{"body = 1", "text never equals a number, and a null body is still null rather than false"},
		{"NOT (body = 1)", "NOT null is null, so a null body must not be matched by the negation either"},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			scope := &store.RowScope{Owner: "alice"}
			result, err := s.Delete(ctx, "app", "notes", tc.filter, nil, store.DeleteOpts{DryRun: true}, scope, store.Incarnation{})
			if err != nil {
				t.Fatalf("the engine owes this filter an evaluation: %v", err)
			}
			if result.Matched != 0 {
				t.Fatalf("%s matched %d rows: %s", tc.filter, result.Matched, tc.why)
			}
		})
	}
}
