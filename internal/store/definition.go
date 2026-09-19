package store

import (
	"fmt"

	"github.com/lsm/dolmen/internal/schema"
)

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
