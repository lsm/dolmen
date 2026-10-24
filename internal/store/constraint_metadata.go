package store

import (
	"context"
	"fmt"

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
