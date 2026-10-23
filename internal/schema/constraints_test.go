package schema

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/filter"
)

var constraintFields = []Field{
	{Name: "slug", Type: String},
	{Name: "body", Type: Text},
	{Name: "project_id", Type: Number},
	{Name: "retired_at", Type: Timestamp},
	{Name: "active", Type: Boolean},
	{Name: "payload", Type: JSON},
	{Name: "embedding", Type: Vector, Dim: 3},
	{Name: "credential", Type: Secret},
	{Name: "attachment", Type: File},
}

func uniqueDeclaration(where string) Constraint {
	return Constraint{Name: "active_slug", Kind: ConstraintUnique, Fields: []string{"slug"}, Where: where}
}

func foreignKeyDeclaration() Constraint {
	return Constraint{Name: "project_ref", Kind: ConstraintForeignKey, Field: "project_id", References: &ConstraintReference{Table: "projects", Field: "id"}}
}

func TestConstraintDeclarationsAcceptDesignExamples(t *testing.T) {
	examples := []string{
		`{"name":"active_slug","kind":"unique","fields":["slug"],"where":"retired_at IS NULL"}`,
		`{"name":"project_ref","kind":"foreign_key","field":"project_id","references":{"table":"projects","field":"id"}}`,
	}
	var declarations []Constraint
	for _, example := range examples {
		var c Constraint
		if err := json.Unmarshal([]byte(example), &c); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip Constraint
		if err := json.Unmarshal(encoded, &roundtrip); err != nil || !reflect.DeepEqual(c, roundtrip) {
			t.Fatalf("round trip: %+v, %v", roundtrip, err)
		}
		declarations = append(declarations, c)
	}
	if err := ValidateConstraintDeclarations(context.Background(), constraintFields, declarations); err != nil {
		t.Fatal(err)
	}
}

func TestConstraintPredicatesAcceptOnlyThePortableSubset(t *testing.T) {
	accepted := []string{
		"", "retired_at IS NULL", "retired_at IS NOT NULL",
		"slug = 'AND ? SELECT'", "project_id >= -1", "project_id <= +3",
		"active = TRUE AND (slug <> 'old' OR NOT (body = 'archived'))",
		"project_id = 9223372036854775807", "project_id != NULL",
		"(slug) = ('live')", "retired_at = '2026-10-23T08:00:00-04:00'",
	}
	for _, predicate := range accepted {
		t.Run("accept_"+predicate, func(t *testing.T) {
			if err := ValidateConstraintDeclarations(context.Background(), constraintFields, []Constraint{uniqueDeclaration(predicate)}); err != nil {
				t.Fatal(err)
			}
		})
	}
	rejected := []string{
		" ", "lower(slug) = 'a'", "project_id + 1 > 0", "slug IN ('a', 'b')",
		"slug LIKE 'a%'", "project_id BETWEEN 1 AND 3", "slug || body = 'ab'",
		"CASE WHEN active THEN 1 ELSE 0 END = 1", "slug = ?",
		"slug = 'a'; SELECT 1", "EXISTS (SELECT 1)", "other.slug = 'a'",
		"owner = 'a'", "id = 1", "created_at IS NULL", "_embedding IS NULL",
		"payload IS NULL", "embedding IS NULL", "credential IS NULL", "attachment IS NULL",
		"slug = X'61'", "slug IS 'a'", "active", "1", "NOT slug",
	}
	for _, predicate := range rejected {
		t.Run("reject_"+predicate, func(t *testing.T) {
			if err := ValidateConstraintDeclarations(context.Background(), constraintFields, []Constraint{uniqueDeclaration(predicate)}); err == nil || !strings.Contains(err.Error(), "active_slug") {
				t.Fatalf("predicate %q: %v", predicate, err)
			}
		})
	}
}

func TestConstraintDeclarationsRejectMalformedKeys(t *testing.T) {
	cases := []Constraint{
		{Name: "wrong_kind", Kind: "primary_key", Fields: []string{"slug"}},
		{Name: "Bad", Kind: ConstraintUnique, Fields: []string{"slug"}},
		{Name: "id", Kind: ConstraintUnique, Fields: []string{"slug"}},
		{Name: "select", Kind: ConstraintUnique, Fields: []string{"slug"}},
		{Name: "long_" + strings.Repeat("a", 64), Kind: ConstraintUnique, Fields: []string{"slug"}},
		{Name: "no_key", Kind: ConstraintUnique},
		{Name: "repeated", Kind: ConstraintUnique, Fields: []string{"slug", "slug"}},
		{Name: "unknown", Kind: ConstraintUnique, Fields: []string{"absent"}},
		{Name: "system_key", Kind: ConstraintUnique, Fields: []string{"id"}},
		{Name: "wrong_case", Kind: ConstraintUnique, Fields: []string{"Slug"}},
		{Name: "mixed", Kind: ConstraintUnique, Fields: []string{"slug"}, Field: "project_id"},
		{Name: "mixed_ref", Kind: ConstraintUnique, Fields: []string{"slug"}, References: &ConstraintReference{Table: "projects", Field: "id"}},
	}
	for _, name := range []string{"payload", "embedding", "credential", "attachment"} {
		cases = append(cases, Constraint{Name: "unsupported_" + name, Kind: ConstraintUnique, Fields: []string{name}})
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if err := ValidateConstraintDeclarations(context.Background(), constraintFields, []Constraint{c}); err == nil {
				t.Fatalf("accepted %+v", c)
			}
		})
	}
	c := uniqueDeclaration("")
	if err := ValidateConstraintDeclarations(context.Background(), constraintFields, []Constraint{c, c}); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("duplicate names: %v", err)
	}
}

func TestForeignKeyDeclarationsRejectNonSystemOrCrossNamespaceTargets(t *testing.T) {
	mutations := []func(*Constraint){
		func(c *Constraint) { c.Field = "slug" },
		func(c *Constraint) { c.Field = "absent" },
		func(c *Constraint) { c.Field = "id" },
		func(c *Constraint) { c.Fields = []string{} },
		func(c *Constraint) { c.Where = "active = TRUE" },
		func(c *Constraint) { c.References = nil },
		func(c *Constraint) { c.References.Field = "slug" },
		func(c *Constraint) { c.References.Table = "other/projects" },
		func(c *Constraint) { c.References.Table = "other.projects" },
		func(c *Constraint) { c.References.Table = "" },
		func(c *Constraint) { c.References.Table = "select" },
	}
	for i, mutate := range mutations {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			c := foreignKeyDeclaration()
			mutate(&c)
			if err := ValidateConstraintDeclarations(context.Background(), constraintFields, []Constraint{c}); err == nil || !strings.Contains(err.Error(), "project_ref") {
				t.Fatalf("accepted %+v: %v", c, err)
			}
		})
	}
}

func TestConstraintDeclarationBudgets(t *testing.T) {
	var fields []Field
	for i := 0; i < 9; i++ {
		fields = append(fields, Field{Name: fmt.Sprintf("key_%d", i), Type: Number})
	}
	c := uniqueDeclaration("")
	c.Fields = nil
	for _, f := range fields[:8] {
		c.Fields = append(c.Fields, f.Name)
	}
	if err := ValidateConstraintDeclarations(context.Background(), fields, []Constraint{c}); err != nil {
		t.Fatal(err)
	}
	c.Fields = append(c.Fields, fields[8].Name)
	if err := ValidateConstraintDeclarations(context.Background(), fields, []Constraint{c}); err == nil {
		t.Fatal("accepted nine key fields")
	}
	var declarations []Constraint
	for i := 0; i < 32; i++ {
		c := uniqueDeclaration("")
		c.Name = fmt.Sprintf("constraint_%d", i)
		declarations = append(declarations, c)
	}
	if err := ValidateConstraintDeclarations(context.Background(), constraintFields, declarations); err != nil {
		t.Fatal(err)
	}
	c = uniqueDeclaration("")
	c.Name = "extra_constraint"
	declarations = append(declarations, c)
	if err := ValidateConstraintDeclarations(context.Background(), constraintFields, declarations); err == nil {
		t.Fatal("accepted 33 constraints")
	}
	for _, predicate := range []string{
		"slug = '" + strings.Repeat("a", filter.MaxFilterLength) + "'",
		strings.Repeat("(", filter.MaxNestingDepth+1) + "active = TRUE" + strings.Repeat(")", filter.MaxNestingDepth+1),
	} {
		if err := ValidateConstraintDeclarations(context.Background(), constraintFields, []Constraint{uniqueDeclaration(predicate)}); err == nil {
			t.Fatal("accepted oversized predicate")
		}
	}
}

func TestConstraintDeclarationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateConstraintDeclarations(ctx, constraintFields, []Constraint{uniqueDeclaration("")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled validation: %v", err)
	}
}
