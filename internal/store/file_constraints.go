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

func FileConstraintField(f schema.Field, ch schema.Change) (schema.Field, error) {
	if f.Type != schema.File {
		return f, invalidf("field %q: set_file_constraints is only allowed on file fields (this field has type %s)", f.Name, f.Type)
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
		return f, invalidf("%s", err)
	}
	f.MaxBytes, f.MediaTypes = maxBytes, mediaTypes
	return f, nil
}

func FileRowsRefusal(field string, violating int64, sample []int64, scoped bool) error {
	if scoped {
		return invalidf("field %q: cannot apply these file constraints — rows hold files that break them; clear or replace those files first, or loosen the constraints. Which rows, and how many, is reported only to a caller holding read on the table", field)
	}
	ids := make([]string, len(sample))
	for i, id := range sample {
		ids[i] = fmt.Sprint(id)
	}
	return invalidf("field %q: cannot apply these file constraints — %d rows hold files that break them (ids %s); clear or replace those files first (update with set %s = null), or loosen the constraints", field, violating, strings.Join(ids, ", "), field)
}

func setFileConstraints(ctx context.Context, db querier, table, phys string, f *schema.Field, ch schema.Change, scoped bool) error {
	next, err := FileConstraintField(*f, ch)
	if err != nil {
		return err
	}
	if phys != "" {
		rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT id, json_extract(%s, '$.size'), json_extract(%s, '$.media_type') FROM %s WHERE %s IS NOT NULL ORDER BY id`, q(phys), q(phys), q(table), q(phys)))
		if err != nil {
			return err
		}
		defer rows.Close()
		var violating int64
		var sample []int64
		for rows.Next() {
			var id, size int64
			var mt sql.NullString
			if err := rows.Scan(&id, &size, &mt); err != nil {
				return err
			}
			if (next.MaxBytes > 0 && size > next.MaxBytes) || !schema.MediaTypeAllowed(next.MediaTypes, mt.String) {
				violating++
				if len(sample) < 5 {
					sample = append(sample, id)
				}
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if violating > 0 {
			return FileRowsRefusal(f.Name, violating, sample, scoped)
		}
	}
	*f = next
	return nil
}
