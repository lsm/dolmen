package schema

import "fmt"

type Relation struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
}

func ValidateRelation(fields []Field, constraints []Constraint, r *Relation) error {
	if r == nil {
		return nil
	}
	if r.Source == "" || r.Target == "" || r.Type == "" || r.Source == r.Target || r.Source == r.Type || r.Target == r.Type {
		return fmt.Errorf("relation needs three distinct fields: source, target and type")
	}
	byName := make(map[string]Field, len(fields))
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, role := range []struct{ name, field string }{{"source", r.Source}, {"target", r.Target}} {
		f, ok := byName[role.field]
		if !ok || f.Type != Number || !f.Required {
			return fmt.Errorf("relation %s %q must name a declared required number field", role.name, role.field)
		}
		if !hasForeignKey(constraints, role.field) {
			return fmt.Errorf("relation %s %q needs a foreign_key constraint on that field naming the endpoint table", role.name, role.field)
		}
	}
	if f, ok := byName[r.Type]; !ok || f.Type != String || !f.Required {
		return fmt.Errorf("relation type %q must name a declared required string field", r.Type)
	}
	return nil
}

func hasForeignKey(constraints []Constraint, field string) bool {
	for _, c := range constraints {
		if c.Kind == ConstraintForeignKey && c.Field == field {
			return true
		}
	}
	return false
}
