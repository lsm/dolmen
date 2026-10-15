package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

func FileConstraintArgs(i int, ch schema.Change) error {
	given := ch.MaxBytes != nil || ch.MediaTypes != nil
	if ch.Op == schema.OpSetFileConstraints && !given {
		return invalidf("changes[%d]: set_file_constraints needs max_bytes (0 removes the limit), media_types (an empty array removes the constraint), or both", i)
	}
	if ch.Op != schema.OpSetFileConstraints && given {
		return invalidf("changes[%d]: max_bytes and media_types are only allowed on set_file_constraints (op %q has none to set)", i, ch.Op)
	}
	return nil
}

func setFileConstraints(ctx context.Context, db querier, table, phys string, f *schema.Field, ch schema.Change, scoped bool) error {
	if f.Type != schema.File {
		return invalidf("field %q: set_file_constraints is only allowed on file fields (this field has type %s)", f.Name, f.Type)
	}
	maxBytes, mediaTypes := f.MaxBytes, f.MediaTypes
	if ch.MaxBytes != nil {
		maxBytes = *ch.MaxBytes
	}
	if ch.MediaTypes != nil {
		mediaTypes = *ch.MediaTypes
		if len(mediaTypes) == 0 {
			mediaTypes = nil
		}
	}
	if err := schema.ValidateFileConstraints(f.Name, maxBytes, mediaTypes); err != nil {
		return invalidf("%s", err)
	}
	if phys != "" {
		rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT id, json_extract(%s, '$.size'), json_extract(%s, '$.media_type') FROM %s WHERE %s IS NOT NULL ORDER BY id`, q(phys), q(phys), q(table), q(phys)))
		if err != nil {
			return err
		}
		defer rows.Close()
		var violating int64
		var sample []string
		for rows.Next() {
			var id, size int64
			var mt sql.NullString
			if err := rows.Scan(&id, &size, &mt); err != nil {
				return err
			}
			if (maxBytes > 0 && size > maxBytes) || !schema.MediaTypeAllowed(mediaTypes, mt.String) {
				violating++
				if len(sample) < 5 {
					sample = append(sample, fmt.Sprint(id))
				}
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if violating > 0 {
			if scoped {
				return invalidf("field %q: cannot apply these file constraints — rows hold files that break them; clear or replace those files first, or loosen the constraints. Which rows, and how many, is reported only to a caller holding read on the table", f.Name)
			}
			return invalidf("field %q: cannot apply these file constraints — %d rows hold files that break them (ids %s); clear or replace those files first (update with set %s = null), or loosen the constraints", f.Name, violating, strings.Join(sample, ", "), f.Name)
		}
	}
	f.MaxBytes, f.MediaTypes = maxBytes, mediaTypes
	return nil
}
