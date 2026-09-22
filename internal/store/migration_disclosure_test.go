package store

import (
	"context"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func seedTwoOwners(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "notes", []schema.Field{
		{Name: "body", Type: schema.Text},
	}, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, who := range []string{"alice", "bob", "bob", "bob"} {
		if _, err := st.Insert(ctx, "ns", "notes", []map[string]any{{"body": "from " + who}},
			WriteOpts{Owner: who}, Embedder{}, nil, Incarnation{}); err != nil {
			t.Fatalf("insert as %s: %v", who, err)
		}
	}
}

func planBackfill(t *testing.T, st *Store, scope *RowScope) int64 {
	t.Helper()
	plan, err := st.PlanMigration(context.Background(), "ns", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}, Default: "x"},
	}, Embedder{}, Incarnation{}, scope, Incarnation{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan.BackfillRows
}

func TestAPlanCountsOnlyTheRowsTheCallerCanSee(t *testing.T) {
	st := openRowAccessStore(t)
	seedTwoOwners(t, st)

	if got := planBackfill(t, st, nil); got != 4 {
		t.Fatalf("a table-wide reader sees every row: backfill_rows %d, want 4", got)
	}
	if got := planBackfill(t, st, &RowScope{Owner: "alice"}); got != 1 {
		t.Fatalf("alice wrote one of the four rows, so the plan may disclose only that: backfill_rows %d, want 1", got)
	}
	if got := planBackfill(t, st, &RowScope{Empty: true}); got != 0 {
		t.Fatalf("a schema-only holder sees no row at all: backfill_rows %d, want 0", got)
	}
}

func TestAPlanStillValidatesAgainstEveryRow(t *testing.T) {
	st := openRowAccessStore(t)
	seedTwoOwners(t, st)

	_, err := st.PlanMigration(context.Background(), "ns", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String, Required: true}},
	}, Embedder{}, Incarnation{}, &RowScope{Empty: true}, Incarnation{})
	if err == nil {
		t.Fatal("a required field with no default cannot be added to a populated table, whatever the caller can see; redacting the count must not redact the rule")
	}
}

func TestAScopedPlanIsGuardedInsideItsTransaction(t *testing.T) {
	st := openRowAccessStore(t)
	seedTwoOwners(t, st)
	ctx := context.Background()

	_, err := st.PlanMigration(ctx, "ns", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}},
	}, Embedder{}, Incarnation{}, &RowScope{Owner: "alice"},
		Incarnation{NsGen: [16]byte{9}, Table: "notes", Version: 1})
	if err == nil {
		t.Fatal("a plan resolved against another namespace incarnation must conflict rather than report on this one")
	}
}

func TestASetEnumRejectionNamesNoValuesToAScopedCaller(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "notes", []schema.Field{
		{Name: "state", Type: schema.String},
	}, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "ns", "notes", []map[string]any{{"state": "wontfix"}},
		WriteOpts{Owner: "bob"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	allowed := []string{"open", "done"}
	change := []schema.Change{{Op: schema.OpSetEnum, Name: "state", Enum: &allowed}}

	_, err := st.PlanMigration(ctx, "ns", "notes", change, Embedder{}, Incarnation{}, nil, Incarnation{})
	if err == nil || !strings.Contains(err.Error(), "wontfix") {
		t.Fatalf("a table-wide reader is told which value blocks the enum: %v", err)
	}

	_, err = st.PlanMigration(ctx, "ns", "notes", change, Embedder{}, Incarnation{}, &RowScope{Empty: true}, Incarnation{})
	if err == nil {
		t.Fatal("the enum is still refused, whoever asks: a rule is not a disclosure")
	}
	if strings.Contains(err.Error(), "wontfix") {
		t.Fatalf("the rejection handed a scoped caller the contents of a field they cannot read: %v", err)
	}
}
