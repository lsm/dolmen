package constraints

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func compileForeign(t *testing.T, own, required bool) *ForeignPlan {
	t.Helper()
	fields := []schema.Field{{Name: "project_id", Type: schema.Number, Required: required}}
	c := schema.Constraint{Name: "project_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: "projects", Field: "id"}}
	plan, err := CompileForeign(context.Background(), fields, c, own, ReferenceTarget{Name: "projects", Own: own, AcceptReferences: true})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestForeignKeyDomainsTreatAbsentAndOtherOwnersIdentically(t *testing.T) {
	plan := compileForeign(t, true, false)
	alice, bob, empty := "alice", "bob", ""
	parents := map[int64]*string{1: &alice, 2: &bob, 3: nil, 4: &empty}
	lookup := func(_ context.Context, table string, id int64) (*string, bool, error) {
		if table != "projects" {
			t.Fatal("lookup escaped its compiled target")
		}
		owner, found := parents[id]
		return owner, found, nil
	}
	for _, tc := range []struct {
		owner *string
		id    int64
		valid bool
	}{{&alice, 1, true}, {&alice, 2, false}, {&alice, 3, false}, {nil, 3, true}, {nil, 1, false}, {nil, 4, false}, {&empty, 4, true}, {&bob, 1, false}, {&alice, 99, false}} {
		err := plan.Check(context.Background(), Row{Values: map[string]any{"project_id": tc.id}, Owner: tc.owner}, lookup)
		if tc.valid && err != nil {
			t.Fatal(err)
		}
		if !tc.valid && !errors.Is(err, derr.ErrConflict) {
			t.Fatalf("expected conflict: %v", err)
		}
		if err != nil && (strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "bob")) {
			t.Fatal("owner leaked")
		}
	}
	wrongOwner := plan.Check(context.Background(), Row{Values: map[string]any{"project_id": int64(2)}, Owner: &alice}, lookup)
	missing := plan.Check(context.Background(), Row{Values: map[string]any{"project_id": int64(99)}, Owner: &alice}, lookup)
	if wrongOwner.Error() != missing.Error() {
		t.Fatal("foreign and absent parents must have identical failures")
	}
}

func TestForeignRequiredNullAndPositiveIDBoundaries(t *testing.T) {
	optional := compileForeign(t, false, false)
	if err := optional.Check(context.Background(), Row{}, nil); err != nil {
		t.Fatal(err)
	}
	required := compileForeign(t, false, true)
	if err := required.Check(context.Background(), Row{}, nil); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("required reference: %v", err)
	}
	lookup := func(_ context.Context, _ string, id int64) (*string, bool, error) {
		return nil, id == math.MaxInt64, nil
	}
	if err := optional.Check(context.Background(), Row{Values: map[string]any{"project_id": int64(math.MaxInt64)}}, lookup); err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{int64(0), int64(-1), 0.5, float64(math.MaxInt64), "private-reference"} {
		err := optional.Check(context.Background(), Row{Values: map[string]any{"project_id": v}}, lookup)
		if !errors.Is(err, derr.ErrConflict) || strings.Contains(err.Error(), "private-reference") {
			t.Fatalf("invalid reference: %v", err)
		}
	}
	if err := optional.Check(context.Background(), Row{Values: map[string]any{"project_id": int64(1)}}, nil); err == nil {
		t.Fatal("missing snapshot lookup must fail closed")
	}
}

func TestParentDeletionUsesCandidateSnapshot(t *testing.T) {
	plan := compileForeign(t, false, true)
	parents := map[int64]bool{1: true}
	lookup := func(_ context.Context, _ string, id int64) (*string, bool, error) { return nil, parents[id], nil }
	child := Row{Values: map[string]any{"project_id": int64(1)}}
	if err := plan.Check(context.Background(), child, lookup); err != nil {
		t.Fatal(err)
	}
	delete(parents, 1)
	if err := plan.Check(context.Background(), child, lookup); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("deleting a referenced parent must fail: %v", err)
	}
}

func TestForeignCompilationRequiresOptInAndMatchingPolicies(t *testing.T) {
	fields := []schema.Field{{Name: "project_id", Type: schema.Number}}
	c := schema.Constraint{Name: "project_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: "projects", Field: "id"}}
	for _, target := range []ReferenceTarget{{Name: "projects"}, {Name: "wrong", AcceptReferences: true}, {Name: "projects", Own: true, AcceptReferences: true}} {
		if _, err := CompileForeign(context.Background(), fields, c, false, target); err == nil {
			t.Fatal("invalid target accepted")
		}
	}
	if _, err := CompileForeign(context.Background(), fields, schema.Constraint{Kind: schema.ConstraintUnique}, false, ReferenceTarget{}); err == nil {
		t.Fatal("non-FK declaration accepted")
	}
}

func TestUniqueSnapshotChecksOwnerDomainsAndPredicateTransitions(t *testing.T) {
	plan, err := compileWhere(context.Background(), "active = TRUE")
	if err != nil {
		t.Fatal(err)
	}
	alice, bob := "alice", "bob"
	rows := []Row{{Values: map[string]any{"slug": "live", "active": true}, Owner: &alice}, {Values: map[string]any{"slug": "live", "active": false}, Owner: &alice}}
	if err := CheckUnique(context.Background(), "active_slug", plan, rows, true); err != nil {
		t.Fatal(err)
	}
	rows[1].Values["active"] = true
	if err := CheckUnique(context.Background(), "active_slug", plan, rows, true); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("predicate transition must conflict: %v", err)
	}
	rows[1].Owner = &bob
	if err := CheckUnique(context.Background(), "active_slug", plan, rows, true); err != nil {
		t.Fatal(err)
	}
	if err := CheckUnique(context.Background(), "active_slug", plan, rows, false); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("plain table must ignore owner labels: %v", err)
	}
	rows[0].Owner, rows[1].Owner = nil, nil
	if err := CheckUnique(context.Background(), "active_slug", plan, rows, true); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("unowned domain must enforce uniqueness: %v", err)
	}
	rows[0].Values["slug"], rows[1].Values["slug"] = nil, nil
	if err := CheckUnique(context.Background(), "active_slug", plan, rows, true); err != nil {
		t.Fatalf("NULL keys must not participate: %v", err)
	}
}

func TestSnapshotChecksPreserveCancellationAndLookupFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := compileForeign(t, false, false)
	if err := plan.Check(ctx, Row{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err := CheckUnique(ctx, "active_slug", nil, nil, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("empty snapshot cancellation: %v", err)
	}
	failure := errors.New("snapshot unavailable")
	err := plan.Check(context.Background(), Row{Values: map[string]any{"project_id": int64(1)}}, func(context.Context, string, int64) (*string, bool, error) { return nil, false, failure })
	if !errors.Is(err, failure) || errors.Is(err, derr.ErrConflict) {
		t.Fatalf("lookup failure must not become an integrity conflict: %v", err)
	}
}
