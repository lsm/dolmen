package constraints

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

var predicateFields = []schema.Field{
	{Name: "slug", Type: schema.String, Enum: []string{"live", "archived"}},
	{Name: "body", Type: schema.Text},
	{Name: "quantity", Type: schema.Number},
	{Name: "active", Type: schema.Boolean},
	{Name: "retired_at", Type: schema.Timestamp},
}

func compileWhere(ctx context.Context, where string) (*UniquePlan, error) {
	return CompileUnique(ctx, predicateFields, schema.Constraint{Name: "active_slug", Kind: schema.ConstraintUnique, Fields: []string{"slug"}, Where: where})
}

func TestCompileUniqueAcceptsTypedPredicates(t *testing.T) {
	for _, where := range []string{
		"", "slug = 'live'", "slug != 'outside_enum'", "slug = body",
		"quantity >= -9223372036854775808", "quantity = 1.0", "1 = quantity",
		"active = TRUE", "active = 1", "active = FALSE",
		"retired_at IS NULL", "retired_at IS NOT NULL",
		"retired_at = '2026-10-23T08:00:00-04:00'", "'2026-10-23' <= retired_at",
		"quantity != NULL", "NULL = quantity", "NULL = 1", "1 = NULL",
		"NULL = NULL", "NULL IS NULL", "1 = 1", "TRUE = FALSE",
		"(quantity > 1 AND active = TRUE) OR NOT (slug = 'archived')",
	} {
		t.Run(where, func(t *testing.T) {
			if _, err := compileWhere(context.Background(), where); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompileUniqueRejectsMalformedOrIncompatibleValues(t *testing.T) {
	for _, where := range []string{
		"quantity = 'not_a_number'", "retired_at = 'private-invalid-date'",
		"slug = quantity", "quantity = active", "active = 'yes'", "active = 2",
		"quantity = 1e999", "slug = 1", "retired_at < quantity",
		"length(slug) = 4", "owner = 'other'", "quantity + 1 > 0",
		"slug = ?", "SELECT 1", "slug = 'a'; SELECT 1",
	} {
		t.Run(where, func(t *testing.T) {
			_, err := compileWhere(context.Background(), where)
			if err == nil || !strings.Contains(err.Error(), "active_slug") {
				t.Fatalf("invalid predicate: %v", err)
			}
			if strings.Contains(err.Error(), "private-invalid-date") {
				t.Fatal("coercion error leaked a value")
			}
		})
	}
	if _, err := CompileUnique(context.Background(), predicateFields, schema.Constraint{Name: "reference", Kind: schema.ConstraintForeignKey}); err == nil {
		t.Fatal("non-unique declaration accepted")
	}
}

func TestCompiledTimestampLiteralPreservesOffsetAndNormalizesCase(t *testing.T) {
	plan, err := compileWhere(context.Background(), "retired_at = '2026-10-23t08:00:00-04:00'")
	if err != nil {
		t.Fatal(err)
	}
	if plan.predicate.b.constant == nil || plan.predicate.b.constant.text != "2026-10-23T08:00:00-04:00" {
		t.Fatal("literal normalization changed its offset or failed to normalize case")
	}
}

func TestCompileUniqueDoesNotRetainMutableDeclarationSlices(t *testing.T) {
	fields := append([]schema.Field{}, predicateFields...)
	declaration := schema.Constraint{Name: "active_slug", Kind: schema.ConstraintUnique, Fields: []string{"slug", "quantity"}}
	plan, err := CompileUnique(context.Background(), fields, declaration)
	if err != nil {
		t.Fatal(err)
	}
	fields[0].Name, declaration.Fields[0] = "replaced", "replaced"
	if plan.keys[0].Name != "slug" || plan.keys[1].Name != "quantity" {
		t.Fatal("compiled key changed with its caller's slices")
	}
}

func TestCompileUniqueCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := compileWhere(ctx, "slug = 'live'"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestLiteralComparisonCompatibilityDoesNotDependOnOrder(t *testing.T) {
	for _, pair := range [][2]string{{"TRUE", "1"}, {"FALSE", "0"}, {"'1'", "1"}, {"'true'", "TRUE"}} {
		for _, where := range []string{pair[0] + " = " + pair[1], pair[1] + " = " + pair[0]} {
			if _, err := compileWhere(t.Context(), where); err == nil {
				t.Fatalf("mixed literal types accepted: %s", where)
			}
		}
	}
	for _, where := range []string{"1 = 1.0", "NULL = TRUE", "TRUE = NULL", "NULL = 'x'", "'x' = NULL", "'x' <> 'y'"} {
		if _, err := compileWhere(t.Context(), where); err != nil {
			t.Fatalf("compatible comparison %s: %v", where, err)
		}
	}
}
