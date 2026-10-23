package store

import (
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
