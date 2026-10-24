package constraints

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestPredicateEvaluationUsesThreeValuedLogic(t *testing.T) {
	states := []struct {
		number  any
		boolean any
		value   truth
	}{{int64(1), true, yes}, {int64(0), false, no}, {nil, nil, unknown}}
	for _, left := range states {
		for _, right := range states {
			for _, op := range []string{"and", "or"} {
				t.Run(fmt.Sprintf("%d_%s_%d", left.value, op, right.value), func(t *testing.T) {
					plan, err := compileWhere(context.Background(), "quantity = 1 "+op+" active = TRUE")
					if err != nil {
						t.Fatal(err)
					}
					want := unknown
					if op == "and" {
						if left.value == no || right.value == no {
							want = no
						} else if left.value == yes && right.value == yes {
							want = yes
						}
					} else if left.value == yes || right.value == yes {
						want = yes
					} else if left.value == no && right.value == no {
						want = no
					}
					row := map[string]any{"quantity": left.number, "active": right.boolean, "slug": "live"}
					got, err := evaluate(context.Background(), plan.predicate, row)
					if err != nil || got != want {
						t.Fatalf("truth=%d, want=%d: %v", got, want, err)
					}
					_, participates, err := plan.Key(context.Background(), row, nil)
					if err != nil || participates != (want == yes) {
						t.Fatalf("participation=%v: %v", participates, err)
					}
				})
			}
		}
	}
	for _, state := range states {
		plan, err := compileWhere(context.Background(), "NOT (quantity = 1)")
		if err != nil {
			t.Fatal(err)
		}
		got, err := evaluate(context.Background(), plan.predicate, map[string]any{"quantity": state.number})
		want := unknown
		if state.value != unknown {
			want = yes - state.value
		}
		if err != nil || got != want {
			t.Fatalf("NOT %d: %d, %v", state.value, got, err)
		}
	}
}

func TestTypedPredicateComparisonsAndNullParticipation(t *testing.T) {
	cases := []struct {
		where  string
		column string
		input  any
		want   bool
	}{
		{"quantity > 9007199254740992", "quantity", int64(9007199254740993), true},
		{"quantity = 9223372036854775807", "quantity", int64(math.MaxInt64), true},
		{"quantity = 9223372036854775807", "quantity", float64(math.MaxInt64), false},
		{"quantity < 0", "quantity", int64(-1), true},
		{"quantity <= -1", "quantity", int64(-1), true},
		{"quantity >= 1", "quantity", float64(1), true},
		{"quantity == 0", "quantity", math.Copysign(0, -1), true},
		{"quantity <> 1", "quantity", int64(2), true},
		{"quantity != 1", "quantity", int64(1), false},
		{"quantity != NULL", "quantity", int64(1), false},
		{"retired_at IS NULL", "retired_at", nil, true},
		{"retired_at IS NOT NULL", "retired_at", nil, false},
		{"retired_at IS NOT NULL", "retired_at", "2026-10-23", true},
		{"retired_at = '2026-10-23T12:00:00Z'", "retired_at", "2026-10-23T08:00:00-04:00", false},
		{"retired_at < '2026-10-23T12:00:00Z'", "retired_at", "2026-10-23T08:00:00-04:00", true},
		{"retired_at = '2026-10-23T12:00:00Z'", "retired_at", "2026-10-23t12:00:00z", true},
		{"slug = body", "body", "live", true},
		{"slug != body", "body", "LIVE", true},
		{"active = TRUE", "active", int64(1), true},
		{"active > FALSE", "active", true, true},
		{"NULL IS NULL", "quantity", nil, true},
		{"NULL = 1", "quantity", nil, false},
	}
	for _, tt := range cases {
		t.Run(tt.where, func(t *testing.T) {
			plan, err := compileWhere(context.Background(), tt.where)
			if err != nil {
				t.Fatal(err)
			}
			_, participates, err := plan.Key(context.Background(), map[string]any{"slug": "live", tt.column: tt.input}, nil)
			if err != nil || participates != tt.want {
				t.Fatalf("participation=%v, want=%v: %v", participates, tt.want, err)
			}
		})
	}
	plan, err := compileWhere(context.Background(), "quantity = 1")
	if err != nil {
		t.Fatal(err)
	}
	if _, participates, err := plan.Key(context.Background(), map[string]any{"slug": nil, "quantity": int64(1)}, nil); err != nil || participates {
		t.Fatalf("NULL key must not participate: %v, %v", participates, err)
	}
}

func TestEvaluationCancellationAndValueRedaction(t *testing.T) {
	plan, err := compileWhere(context.Background(), "quantity > 1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := plan.Key(ctx, map[string]any{"slug": "live", "quantity": int64(2)}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	for _, input := range []any{"private-value", math.NaN(), math.Inf(1)} {
		_, _, err := plan.Key(context.Background(), map[string]any{"slug": "live", "quantity": input}, nil)
		if err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("invalid value error: %v", err)
		}
	}
	var missing *UniquePlan
	if _, _, err := missing.Key(context.Background(), nil, nil); err == nil {
		t.Fatal("missing plan must fail closed")
	}
}

func TestCompiledPlanCanBeReusedConcurrently(t *testing.T) {
	plan, err := compileWhere(context.Background(), "quantity > 9007199254740992 AND retired_at IS NULL")
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"slug": "live", "quantity": int64(9007199254740993)}
	want, _, err := plan.Key(context.Background(), row, nil)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 100; j++ {
				got, participates, err := plan.Key(context.Background(), row, nil)
				if err != nil || !participates || got != want {
					t.Errorf("compiled plan changed: %v, %v", participates, err)
					return
				}
			}
		}()
	}
	group.Wait()
}

func TestUnconditionalCompiledKeyPreservesOwnerDomain(t *testing.T) {
	plan, err := CompileUnique(context.Background(), predicateFields, schema.Constraint{Name: "all_slugs", Kind: schema.ConstraintUnique, Fields: []string{"slug"}})
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"slug": "outside_enum"}
	alice := "alice"
	unowned, _, err := plan.Key(context.Background(), row, nil)
	if err != nil {
		t.Fatal(err)
	}
	owned, _, err := plan.Key(context.Background(), row, &alice)
	if err != nil || unowned == owned {
		t.Fatalf("owner domain collapsed: %v", err)
	}
}

func TestNonfiniteKeysAreRejectedOutsideThePredicate(t *testing.T) {
	plan, err := CompileUnique(t.Context(), predicateFields, schema.Constraint{Name: "active_quantity", Kind: schema.ConstraintUnique, Fields: []string{"slug", "quantity"}, Where: "active = TRUE"})
	if err != nil {
		t.Fatal(err)
	}
	for _, active := range []any{false, nil} {
		for _, n := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			_, participates, err := plan.Key(t.Context(), map[string]any{"slug": nil, "quantity": n, "active": active}, nil)
			if err == nil || participates {
				t.Fatalf("non-finite key accepted outside predicate: active=%v, participation=%v, err=%v", active, participates, err)
			}
		}
	}
}
