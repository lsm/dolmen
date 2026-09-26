package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

func ShapeChangeArgs(i int, ch schema.Change) error {
	if ch.Op == schema.OpSetShape && ch.Shape == nil {
		return invalidf("changes[%d]: set_shape requires an explicit shape (one of %s), or an empty string to remove the constraint", i, strings.Join(schema.Shapes, ", "))
	}
	if ch.Op != schema.OpSetShape && ch.Shape != nil {
		return invalidf("changes[%d]: shape is only allowed on set_shape (op %q has no shape to set)", i, ch.Op)
	}
	return nil
}

func ShapeTarget(f *schema.Field, shape string) error {
	if f.Type != schema.JSON {
		return invalidf("field %q: shape is only allowed on json fields (this field has type %s)", f.Name, f.Type)
	}
	if shape == "" {
		return nil
	}
	if err := schema.ValidateShape(f.Name, shape); err != nil {
		return invalidf("%s", err)
	}
	return nil
}

func ShapeDefaults(f *schema.Field, shape string, backfill any) error {
	if shape == "" {
		return nil
	}
	if f.Default != nil {
		raw, err := json.Marshal(f.Default)
		if err != nil {
			return invalidf("field %q: cannot marshal the declared default: %v", f.Name, err)
		}
		if schema.ShapeViolation(f.Name, shape, raw) != nil {
			return invalidf("field %q: the declared default %s does not fit shape %s; change the default, or pick a shape it fits", f.Name, raw, shape)
		}
	}
	if text, ok := backfill.(string); ok {
		if schema.ShapeViolation(f.Name, shape, []byte(text)) != nil {
			return invalidf("field %q: the add_field backfill default %s does not fit shape %s; pick a backfill that fits, or a shape it fits", f.Name, text, shape)
		}
	}
	return nil
}

func ShapeFits(shape, stored string) bool {
	return schema.ShapeViolation("", shape, []byte(stored)) == nil
}

func ShapeRowsRefusal(field, shape string, violating int64, sample []int64, scoped bool) error {
	if scoped {
		return invalidf("field %q: cannot apply shape %s — rows hold values that do not fit it; rewrite them to fit first (update with set %s = ...), or pick a shape they fit. Which rows, and how many, is reported only to a caller holding read on the table", field, shape, field)
	}
	ids := make([]string, len(sample))
	for i, id := range sample {
		ids[i] = fmt.Sprint(id)
	}
	return invalidf("field %q: cannot apply shape %s — %d rows hold values that do not fit it (ids %s); rewrite those rows to fit first (update with set %s = ...), or pick a shape they fit", field, shape, violating, strings.Join(ids, ", "), field)
}

func ShapeOperation(name, shape string) string {
	if shape == "" {
		return "set_shape " + name + " = (none)"
	}
	return "set_shape " + name + " = " + shape
}
