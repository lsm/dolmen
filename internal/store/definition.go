package store

import (
	"fmt"

	"github.com/lsm/dolmen/internal/schema"
)

func ValidateOwnerCollision(fields []schema.Field) error {
	for _, f := range fields {
		if schema.ReservedWithOwner(f.Name) {
			return invalidf("field %q collides with the implicit owner column this table carries because it declares row_access; rename the field, for example to %q", schema.OwnerColumn, "owner_name")
		}
	}
	return nil
}

func ValidateTableDefinition(table string, fields []schema.Field) ([]schema.Field, error) {
	if err := schema.ValidateTableName(table); err != nil {
		return nil, invalidf("%s", err)
	}
	if len(fields) > MaxFieldsPerTable {
		return nil, invalidf("too many fields: %d (max %d; SQLite caps tables at 2000 columns including the implicit id, created_at, and _embedding)", len(fields), MaxFieldsPerTable)
	}
	fields = schema.Normalize(fields)
	if err := schema.Validate(fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := validateFieldDefaults(fields); err != nil {
		return nil, err
	}
	return fields, nil
}
