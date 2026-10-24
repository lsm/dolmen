package schema

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/lsm/dolmen/internal/filter"
)

const (
	ConstraintUnique          = "unique"
	ConstraintForeignKey      = "foreign_key"
	MaxConstraintsPerTable    = 32
	MaxUniqueConstraintFields = 8
)

type ConstraintReference struct {
	Table string `json:"table"`
	Field string `json:"field"`
}

type Constraint struct {
	Name       string               `json:"name"`
	Kind       string               `json:"kind"`
	Fields     []string             `json:"fields,omitempty"`
	Where      string               `json:"where,omitempty"`
	Field      string               `json:"field,omitempty"`
	References *ConstraintReference `json:"references,omitempty"`
}

func constraintScalar(t FieldType) bool {
	switch t {
	case String, Text, Number, Boolean, Timestamp:
		return true
	}
	return false
}

func ValidateConstraintDeclarations(ctx context.Context, fields []Field, constraints []Constraint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(constraints) > MaxConstraintsPerTable {
		return fmt.Errorf("at most %d constraints are allowed per table; remove declarations", MaxConstraintsPerTable)
	}
	byName := make(map[string]Field, len(fields))
	var columns []string
	for _, f := range fields {
		byName[f.Name] = f
		if constraintScalar(f.Type) {
			columns = append(columns, f.Name)
		}
	}
	names := make(map[string]bool, len(constraints))
	for i, c := range constraints {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ValidateIdent(c.Name, "constraint name"); err != nil {
			return fmt.Errorf("constraints[%d]: %w", i, err)
		}
		if names[c.Name] {
			return fmt.Errorf("constraint %q is declared twice; use distinct names", c.Name)
		}
		names[c.Name] = true
		var err error
		switch c.Kind {
		case ConstraintUnique:
			err = validateUniqueConstraint(ctx, byName, columns, c)
		case ConstraintForeignKey:
			err = validateForeignKeyDeclaration(byName, c)
		default:
			err = fmt.Errorf("kind must be unique or foreign_key; primary keys remain system-managed")
		}
		if err != nil {
			return fmt.Errorf("constraint %q: %w", c.Name, err)
		}
	}
	return nil
}

func validateUniqueConstraint(ctx context.Context, fields map[string]Field, columns []string, c Constraint) error {
	if c.Field != "" || c.References != nil {
		return fmt.Errorf("unique takes fields and optional where, not field or references")
	}
	if len(c.Fields) < 1 || len(c.Fields) > MaxUniqueConstraintFields {
		return fmt.Errorf("fields must contain 1 to %d distinct scalar fields", MaxUniqueConstraintFields)
	}
	seen := make(map[string]bool, len(c.Fields))
	for _, name := range c.Fields {
		f, ok := fields[name]
		if !ok || !constraintScalar(f.Type) {
			return fmt.Errorf("key field %q must name a declared string, text, number, boolean or timestamp field", name)
		}
		if seen[name] {
			return fmt.Errorf("key field %q is repeated; list each field once", name)
		}
		seen[name] = true
	}
	if c.Where == "" {
		return nil
	}
	node, err := filter.Parse(c.Where, filter.Options{Context: ctx, Columns: columns})
	if err != nil {
		return fmt.Errorf("where: %w", err)
	}
	if !constraintPredicate(node) {
		return fmt.Errorf("where allows only scalar comparisons, IS [NOT] NULL, AND, OR, NOT and parentheses; remove other expressions")
	}
	return nil
}

func validateForeignKeyDeclaration(fields map[string]Field, c Constraint) error {
	if c.Fields != nil || c.Where != "" {
		return fmt.Errorf("foreign_key takes field and references, not fields or where")
	}
	if f, ok := fields[c.Field]; !ok || f.Type != Number {
		return fmt.Errorf("field %q must name a declared number field holding system IDs", c.Field)
	}
	if c.References == nil || c.References.Field != "id" {
		return fmt.Errorf("references must name a same-namespace table and its system id field")
	}
	return ValidateTableName(c.References.Table)
}

func constraintPredicate(n filter.Node) bool {
	switch n := n.(type) {
	case *filter.Paren:
		return constraintPredicate(n.Inner)
	case *filter.Unary:
		return n.Op == "not" && constraintPredicate(n.Operand)
	case *filter.Binary:
		switch n.Op {
		case "and", "or":
			return constraintPredicate(n.Left) && constraintPredicate(n.Right)
		case "=", "==", "!=", "<>", "<", "<=", ">", ">=":
			return constraintOperand(n.Left) && constraintOperand(n.Right)
		}
	case *filter.Is:
		lit, ok := n.Right.(*filter.Literal)
		return ok && lit.Kind == filter.LiteralNull && constraintOperand(n.Left)
	}
	return false
}

func constraintOperand(n filter.Node) bool {
	switch n := n.(type) {
	case *filter.Column:
		return true
	case *filter.Literal:
		return n.Kind != filter.LiteralBlob
	case *filter.Paren:
		return constraintOperand(n.Inner)
	case *filter.Unary:
		lit, ok := n.Operand.(*filter.Literal)
		return ok && lit.Kind == filter.LiteralNumber && (n.Op == "+" || n.Op == "-")
	}
	return false
}

func ConstraintNamingField(ctx context.Context, fields []Field, constraints []Constraint, field string) string {
	columns := make([]string, 0, len(fields))
	for _, f := range fields {
		columns = append(columns, f.Name)
	}
	for _, c := range constraints {
		if c.Field == field || slices.Contains(c.Fields, field) {
			return c.Name
		}
		if c.Where == "" {
			continue
		}
		if node, err := filter.Parse(c.Where, filter.Options{Context: ctx, Columns: columns}); err == nil && mentionsColumn(node, field) {
			return c.Name
		}
	}
	return ""
}

func mentionsColumn(n filter.Node, field string) bool {
	switch n := n.(type) {
	case *filter.Column:
		return strings.EqualFold(n.Name, field)
	case *filter.Paren:
		return mentionsColumn(n.Inner, field)
	case *filter.Unary:
		return mentionsColumn(n.Operand, field)
	case *filter.Binary:
		return mentionsColumn(n.Left, field) || mentionsColumn(n.Right, field)
	case *filter.Is:
		return mentionsColumn(n.Left, field) || mentionsColumn(n.Right, field)
	}
	return false
}

func ReferencingTables(schemas []*TableSchema, table string) []string {
	var out []string
	for _, sc := range schemas {
		if sc == nil || sc.Name == table {
			continue
		}
		for _, c := range sc.Constraints {
			if c.Kind == ConstraintForeignKey && c.References != nil && c.References.Table == table {
				out = append(out, sc.Name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
