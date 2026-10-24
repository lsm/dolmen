package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/lsm/dolmen/internal/schema"
)

func RefuseConstraintOptions(opts TableOpts) error {
	if len(opts.Constraints) > 0 || opts.AcceptReferences {
		return fmt.Errorf("%w: constraints and accept_references are not enforced by this binary; omit these options until enforcement is available", ErrInvalid)
	}
	return nil
}

func RefuseUnenforcedConstraints(sc *schema.TableSchema) error {
	if len(sc.Constraints) > 0 || sc.AcceptReferences {
		return fmt.Errorf("%w: %w: this table requires constraint enforcement; upgrade dolmen to a binary that enforces constraints before using this table", ErrInvalid, ErrCatalogTooNew)
	}
	return nil
}

func RefuseReferencedDrop(table string, referencing []string) error {
	if len(referencing) > 0 {
		return invalidf("table %s is referenced by a foreign key declared on table %s; drop that constraint first", table, referencing[0])
	}
	return nil
}

func RefuseConstrainedField(ctx context.Context, sc *schema.TableSchema, op, field string) error {
	if name := schema.ConstraintNamingField(ctx, sc.Fields, sc.Constraints, field); name != "" {
		return invalidf("%s %s: constraint %q names this field; drop the constraint first", op, field, name)
	}
	return nil
}

func RefuseConstrainedRowAccess(sc *schema.TableSchema, referencing []string) error {
	if len(sc.Constraints) > 0 || len(referencing) > 0 {
		return invalidf("set_row_access: table %s has constraints or incoming foreign keys; drop them first, since both endpoints of a foreign key must share one row_access policy", sc.Name)
	}
	return nil
}

func PlanConstraintChange(ctx context.Context, cur *schema.TableSchema, ch schema.Change, referencing func() ([]string, error)) error {
	switch ch.Op {
	case schema.OpAddConstraint:
		next := append(slices.Clone(cur.Constraints), *ch.Constraint)
		if err := schema.ValidateConstraintDeclarations(ctx, cur.Fields, next); err != nil {
			return fmt.Errorf("%w: add_constraint: %w", ErrInvalid, err)
		}
		cur.Constraints = next
	case schema.OpDropConstraint:
		i := slices.IndexFunc(cur.Constraints, func(c schema.Constraint) bool { return c.Name == ch.Name })
		if i < 0 {
			return invalidf("drop_constraint: table %s has no constraint %q; describe_table lists its constraints", cur.Name, ch.Name)
		}
		cur.Constraints = slices.Delete(slices.Clone(cur.Constraints), i, i+1)
	case schema.OpSetAcceptReferences:
		if !*ch.Value {
			tables, err := referencing()
			if err != nil {
				return err
			}
			if len(tables) > 0 {
				return invalidf("set_accept_references false: table %s is still referenced by a foreign key declared on table %s; drop that constraint first", cur.Name, tables[0])
			}
		}
		cur.AcceptReferences = *ch.Value
	}
	return nil
}

func DescribeConstraintChange(ch schema.Change) string {
	switch ch.Op {
	case schema.OpAddConstraint:
		return fmt.Sprintf("add_constraint %s (%s; existing rows are checked when applied)", ch.Constraint.Name, ch.Constraint.Kind)
	case schema.OpDropConstraint:
		return "drop_constraint " + ch.Name
	}
	return fmt.Sprintf("set_accept_references %v", *ch.Value)
}

func checkMigratedConstraints(ctx context.Context, tx *sql.Tx, old, cur *schema.TableSchema) error {
	if len(cur.Constraints) == 0 && !cur.AcceptReferences && !old.AcceptReferences {
		return nil
	}
	if len(cur.Constraints) > 0 || cur.AcceptReferences {
		if err := requireConstraintReader(ctx, tx); err != nil {
			return err
		}
	}
	return checkSQLiteConstraints(ctx, tx, cur, true)
}

func RefuseMixedConstraintChanges(changes []schema.Change) error {
	adds, fieldOp := false, ""
	for _, ch := range changes {
		switch ch.Op {
		case schema.OpAddConstraint:
			adds = true
		case schema.OpAddField, schema.OpRenameField, schema.OpDropField:
			fieldOp = ch.Op
		}
	}
	if adds && fieldOp != "" {
		return invalidf("add_constraint cannot share a migration with %s; apply the field change first, then add the constraint in its own migrate call", fieldOp)
	}
	return nil
}
