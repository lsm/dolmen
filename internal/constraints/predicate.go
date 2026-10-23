package constraints

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lsm/dolmen/internal/filter"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/value"
)

type operand struct {
	column   string
	field    schema.Field
	constant *scalar
}

type condition struct {
	op          string
	left, right *condition
	a, b        operand
}

type UniquePlan struct {
	keys      []schema.Field
	predicate *condition
}

func CompileUnique(ctx context.Context, fields []schema.Field, c schema.Constraint) (*UniquePlan, error) {
	if c.Kind != schema.ConstraintUnique {
		return nil, fmt.Errorf("CompileUnique requires a unique constraint")
	}
	if err := schema.ValidateConstraintDeclarations(ctx, fields, []schema.Constraint{c}); err != nil {
		return nil, err
	}
	byName := make(map[string]schema.Field, len(fields))
	var columns []string
	for _, f := range fields {
		byName[f.Name] = f
		columns = append(columns, f.Name)
	}
	plan := &UniquePlan{}
	for _, name := range c.Fields {
		f := byName[name]
		f.Enum = nil
		plan.keys = append(plan.keys, f)
	}
	if c.Where == "" {
		return plan, nil
	}
	node, err := filter.Parse(c.Where, filter.Options{Context: ctx, Columns: columns})
	if err != nil {
		return nil, err
	}
	plan.predicate, err = compileCondition(ctx, node, byName)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("constraint %q: where must compare compatible scalar values and valid timestamp literals", c.Name)
	}
	return plan, nil
}

func unparen(n filter.Node) filter.Node {
	for {
		p, ok := n.(*filter.Paren)
		if !ok {
			return n
		}
		n = p.Inner
	}
}

func compileCondition(ctx context.Context, n filter.Node, fields map[string]schema.Field) (*condition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &condition{}
	var a, b filter.Node
	switch n := unparen(n).(type) {
	case *filter.Unary:
		out.op = n.Op
		left, err := compileCondition(ctx, n.Operand, fields)
		out.left = left
		return out, err
	case *filter.Binary:
		out.op = n.Op
		if n.Op == "and" || n.Op == "or" {
			var err error
			out.left, err = compileCondition(ctx, n.Left, fields)
			if err != nil {
				return nil, err
			}
			out.right, err = compileCondition(ctx, n.Right, fields)
			return out, err
		}
		a, b = n.Left, n.Right
	case *filter.Is:
		out.op = "is_null"
		if n.Negated {
			out.op = "is_not_null"
		}
		a, b = n.Left, n.Right
	default:
		return nil, fmt.Errorf("unsupported condition")
	}
	field := schema.Field{Name: "predicate", Type: schema.String}
	for _, node := range []filter.Node{b, a} {
		if col, ok := unparen(node).(*filter.Column); ok {
			field = fields[col.Name]
		}
	}
	if _, ok := unparen(a).(*filter.Column); !ok {
		if _, ok := unparen(b).(*filter.Column); !ok {
			var kind schema.FieldType
			for _, node := range []filter.Node{a, b} {
				raw, err := literalValue(node)
				if err != nil || raw == nil {
					continue
				}
				inferred := schema.String
				switch raw.(type) {
				case int64, float64:
					inferred = schema.Number
				case bool:
					inferred = schema.Boolean
				}
				if kind != "" && kind != inferred {
					return nil, fmt.Errorf("incompatible literal types")
				}
				kind, field.Type = inferred, inferred
			}
		}
	}
	field.Enum = nil
	var err error
	out.a, err = compileOperand(a, field, fields)
	if err != nil {
		return nil, err
	}
	out.b, err = compileOperand(b, field, fields)
	return out, err
}

func compileOperand(n filter.Node, field schema.Field, fields map[string]schema.Field) (operand, error) {
	if col, ok := unparen(n).(*filter.Column); ok {
		actual := fields[col.Name]
		textPair := (actual.Type == schema.String || actual.Type == schema.Text) && (field.Type == schema.String || field.Type == schema.Text)
		if actual.Type != field.Type && !textPair {
			return operand{}, fmt.Errorf("incompatible field types")
		}
		actual.Enum = nil
		return operand{column: col.Name, field: actual}, nil
	}
	raw, err := literalValue(n)
	if err != nil || raw == nil {
		return operand{}, err
	}
	s, err := coerceScalar(field, raw)
	return operand{constant: &s}, err
}

func literalValue(n filter.Node) (any, error) {
	n = unparen(n)
	sign := ""
	if unary, ok := n.(*filter.Unary); ok {
		sign, n = unary.Op, unary.Operand
	}
	lit, ok := n.(*filter.Literal)
	if !ok {
		return nil, fmt.Errorf("expected a scalar literal")
	}
	switch lit.Kind {
	case filter.LiteralNull:
		return nil, nil
	case filter.LiteralString:
		return lit.Text, nil
	case filter.LiteralTrue, filter.LiteralFalse:
		return lit.Kind == filter.LiteralTrue, nil
	case filter.LiteralNumber:
		return value.Coerce(schema.Field{Name: "predicate", Type: schema.Number}, json.Number(sign+lit.Text))
	}
	return nil, fmt.Errorf("unsupported literal")
}
