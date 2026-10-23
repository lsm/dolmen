package constraints

import (
	"context"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

type truth int8

const (
	unknown truth = -1
	no      truth = 0
	yes     truth = 1
)

func (p *UniquePlan) Key(ctx context.Context, row map[string]any, owner *string) (string, bool, error) {
	if p == nil {
		return "", false, fmt.Errorf("a unique constraint must be compiled before evaluation")
	}
	matches, err := evaluate(ctx, p.predicate, row)
	if err != nil || matches != yes {
		return "", false, err
	}
	return UniqueKey(p.keys, row, owner)
}

func evaluate(ctx context.Context, c *condition, row map[string]any) (truth, error) {
	if err := ctx.Err(); err != nil {
		return unknown, err
	}
	if c == nil {
		return yes, nil
	}
	if c.op == "and" || c.op == "or" || c.op == "not" {
		left, err := evaluate(ctx, c.left, row)
		if err != nil {
			return unknown, err
		}
		if c.op == "not" {
			if left == unknown {
				return unknown, nil
			}
			return yes - left, nil
		}
		right, err := evaluate(ctx, c.right, row)
		if err != nil {
			return unknown, err
		}
		if c.op == "and" && (left == no || right == no) {
			return no, nil
		}
		if c.op == "or" && (left == yes || right == yes) {
			return yes, nil
		}
		if left == unknown || right == unknown {
			return unknown, nil
		}
		return left, nil
	}
	a, err := readOperand(c.a, row)
	if err != nil {
		return unknown, err
	}
	if c.op == "is_null" || c.op == "is_not_null" {
		return truthOf((a == nil) == (c.op == "is_null")), nil
	}
	b, err := readOperand(c.b, row)
	if err != nil {
		return unknown, err
	}
	if a == nil || b == nil {
		return unknown, nil
	}
	if a.kind != b.kind {
		return unknown, fmt.Errorf("constraint comparison requires compatible scalar types")
	}
	cmp := strings.Compare(a.text, b.text)
	if a.kind == schema.Number {
		cmp = a.number.Cmp(b.number)
	}
	switch c.op {
	case "=", "==":
		return truthOf(cmp == 0), nil
	case "!=", "<>":
		return truthOf(cmp != 0), nil
	case "<":
		return truthOf(cmp < 0), nil
	case "<=":
		return truthOf(cmp <= 0), nil
	case ">":
		return truthOf(cmp > 0), nil
	case ">=":
		return truthOf(cmp >= 0), nil
	}
	return unknown, fmt.Errorf("unsupported constraint comparison")
}

func truthOf(v bool) truth {
	if v {
		return yes
	}
	return no
}

func readOperand(op operand, row map[string]any) (*scalar, error) {
	if op.column == "" {
		return op.constant, nil
	}
	input := row[op.column]
	if input == nil {
		return nil, nil
	}
	s, err := coerceScalar(op.field, input)
	return &s, err
}
