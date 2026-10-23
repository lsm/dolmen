package constraints

import (
	"context"
	"fmt"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

type Row struct {
	Values map[string]any
	Owner  *string
}

type ReferenceTarget struct {
	Name             string
	Own              bool
	AcceptReferences bool
}

type ParentLookup func(context.Context, string, int64) (*string, bool, error)

type ForeignPlan struct {
	name     string
	field    string
	table    string
	own      bool
	required bool
}

func CompileForeign(ctx context.Context, fields []schema.Field, c schema.Constraint, own bool, target ReferenceTarget) (*ForeignPlan, error) {
	if c.Kind != schema.ConstraintForeignKey {
		return nil, fmt.Errorf("CompileForeign requires a foreign_key constraint")
	}
	if err := schema.ValidateConstraintDeclarations(ctx, fields, []schema.Constraint{c}); err != nil {
		return nil, err
	}
	if target.Name != c.References.Table || !target.AcceptReferences {
		return nil, fmt.Errorf("constraint %q: the referenced table must opt in with accept_references", c.Name)
	}
	if own != target.Own {
		return nil, fmt.Errorf("constraint %q: both endpoints must use the same row_access policy", c.Name)
	}
	plan := &ForeignPlan{name: c.Name, field: c.Field, table: c.References.Table, own: own}
	for _, field := range fields {
		if field.Name == c.Field {
			plan.required = field.Required
			break
		}
	}
	return plan, nil
}

func (p *ForeignPlan) Check(ctx context.Context, row Row, lookup ParentLookup) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("a foreign key must be compiled before evaluation")
	}
	id, present, err := ReferenceID(row.Values[p.field])
	if err != nil || (!present && p.required) {
		return foreignViolation(p.name)
	}
	if !present {
		return nil
	}
	if lookup == nil {
		return fmt.Errorf("foreign-key evaluation requires a parent lookup in the same write snapshot")
	}
	owner, found, err := lookup(ctx, p.table, id)
	if err != nil {
		return err
	}
	if !found || (p.own && !SameOwner(row.Owner, owner)) {
		return foreignViolation(p.name)
	}
	return nil
}

func foreignViolation(name string) error {
	return derr.New(derr.Conflict, "constraint %q is not satisfied; use a valid parent system ID, create the parent before the child, or remove child references before deleting the parent", name)
}

func CheckUnique(ctx context.Context, name string, plan *UniquePlan, rows []Row, own bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan == nil {
		return fmt.Errorf("a unique constraint must be compiled before evaluation")
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		var owner *string
		if own {
			owner = row.Owner
		}
		key, participates, err := plan.Key(ctx, row.Values, owner)
		if err != nil {
			return err
		}
		if !participates {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			return derr.New(derr.Conflict, "constraint %q is not satisfied; change the duplicate key or remove a conflicting record before retrying", name)
		}
		seen[key] = struct{}{}
	}
	return nil
}
