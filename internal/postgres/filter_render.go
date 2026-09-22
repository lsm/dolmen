package postgres

import (
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/filter"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type filterRenderer struct {
	columns map[string]string
	args    []any
	bound   []any
	sb      strings.Builder
}

var renderedFunctions = map[string]string{
	"abs": "abs", "round": "round", "length": "length", "lower": "lower", "upper": "upper",
	"substr": "substr", "trim": "btrim", "ltrim": "ltrim", "rtrim": "rtrim", "replace": "replace",
	"instr": "strpos",
}

func filterNotRenderable(name string) error {
	return store.NewBackendQueryError(fmt.Sprintf("%s is in the filter language this server accepts, but this storage engine cannot evaluate it yet; rewrite the filter without it, or compute the value and bind it as a ? argument", name), nil)
}

func renderScopedFilter(node filter.Node, columns map[string]string, args []any, next int) (string, []any, error) {
	r := &filterRenderer{columns: columns, args: args}
	if err := r.render(node, next); err != nil {
		return "", nil, err
	}
	return r.sb.String(), r.bound, nil
}

func (r *filterRenderer) placeholder(next int) int { return next + len(r.bound) }

func (r *filterRenderer) render(n filter.Node, next int) error {
	switch node := n.(type) {
	case *filter.Column:
		physical, ok := r.columns[node.Name]
		if !ok {
			physical = node.Name
		}
		r.sb.WriteString(ident(physical))
		return nil
	case *filter.Param:
		if node.Index >= len(r.args) {
			return fmt.Errorf("%w: filter argument %d was not supplied", store.ErrInvalid, node.Index+1)
		}
		r.sb.WriteString("$" + fmt.Sprint(r.placeholder(next)))
		r.bound = append(r.bound, r.args[node.Index])
		return nil
	case *filter.Literal:
		return r.literal(node)
	case *filter.Paren:
		r.sb.WriteString("(")
		if err := r.render(node.Inner, next); err != nil {
			return err
		}
		r.sb.WriteString(")")
		return nil
	case *filter.Unary:
		if strings.EqualFold(node.Op, "not") {
			r.sb.WriteString("NOT (")
			if err := r.render(node.Operand, next); err != nil {
				return err
			}
			r.sb.WriteString(")")
			return nil
		}
		r.sb.WriteString("(" + node.Op + " ")
		if err := r.render(node.Operand, next); err != nil {
			return err
		}
		r.sb.WriteString(")")
		return nil
	case *filter.Binary:
		return r.binary(node, next)
	case *filter.Is:
		return r.is(node, next)
	case *filter.In:
		return r.in(node, next)
	case *filter.Like:
		return r.like(node, next)
	case *filter.Between:
		return r.between(node, next)
	case *filter.Call:
		return r.call(node, next)
	case *filter.Case:
		return r.caseExpr(node, next)
	}
	return filterNotRenderable("this expression")
}

func (r *filterRenderer) literal(node *filter.Literal) error {
	switch node.Kind {
	case filter.LiteralNull:
		r.sb.WriteString("NULL")
	case filter.LiteralTrue:
		r.sb.WriteString("TRUE")
	case filter.LiteralFalse:
		r.sb.WriteString("FALSE")
	case filter.LiteralNumber:
		return r.number(node.Text)
	case filter.LiteralBlob:
		r.sb.WriteString("'\\x" + strings.ToLower(node.Text) + "'::bytea")
	default:
		r.sb.WriteString("'" + strings.ReplaceAll(node.Text, "'", "''") + "'")
	}
	return nil
}

func (r *filterRenderer) number(text string) error {
	lowered := strings.ToLower(text)
	if strings.HasPrefix(lowered, "0x") {
		var value int64
		if _, err := fmt.Sscanf(lowered[2:], "%x", &value); err != nil {
			return fmt.Errorf("%w: %q is not a hexadecimal integer", store.ErrInvalid, text)
		}
		r.sb.WriteString(fmt.Sprint(value))
		return nil
	}
	r.sb.WriteString(text)
	return nil
}

func (r *filterRenderer) binary(node *filter.Binary, next int) error {
	op := node.Op
	switch op {
	case "==":
		op = "="
	case "!=":
		op = "<>"
	case "and", "or":
		op = strings.ToUpper(op)
	}
	r.sb.WriteString("(")
	if err := r.render(node.Left, next); err != nil {
		return err
	}
	r.sb.WriteString(" " + op + " ")
	if err := r.render(node.Right, next); err != nil {
		return err
	}
	r.sb.WriteString(")")
	return nil
}

func (r *filterRenderer) is(node *filter.Is, next int) error {
	op := " IS NOT DISTINCT FROM "
	if node.Negated {
		op = " IS DISTINCT FROM "
	}
	r.sb.WriteString("(")
	if err := r.render(node.Left, next); err != nil {
		return err
	}
	r.sb.WriteString(op)
	if err := r.render(node.Right, next); err != nil {
		return err
	}
	r.sb.WriteString(")")
	return nil
}

func (r *filterRenderer) in(node *filter.In, next int) error {
	if len(node.List) == 0 {
		if node.Negated {
			r.sb.WriteString("TRUE")
		} else {
			r.sb.WriteString("FALSE")
		}
		return nil
	}
	r.sb.WriteString("(")
	if err := r.render(node.Left, next); err != nil {
		return err
	}
	if node.Negated {
		r.sb.WriteString(" NOT IN (")
	} else {
		r.sb.WriteString(" IN (")
	}
	for i, item := range node.List {
		if i > 0 {
			r.sb.WriteString(", ")
		}
		if err := r.render(item, next); err != nil {
			return err
		}
	}
	r.sb.WriteString("))")
	return nil
}

func (r *filterRenderer) like(node *filter.Like, next int) error {
	r.sb.WriteString("(")
	if err := r.render(node.Left, next); err != nil {
		return err
	}
	if node.Negated {
		r.sb.WriteString(" NOT LIKE ")
	} else {
		r.sb.WriteString(" LIKE ")
	}
	if err := r.render(node.Pattern, next); err != nil {
		return err
	}
	if node.Escape != nil {
		r.sb.WriteString(" ESCAPE ")
		if err := r.render(node.Escape, next); err != nil {
			return err
		}
	}
	r.sb.WriteString(")")
	return nil
}

func (r *filterRenderer) between(node *filter.Between, next int) error {
	r.sb.WriteString("(")
	if err := r.render(node.Value, next); err != nil {
		return err
	}
	if node.Negated {
		r.sb.WriteString(" NOT BETWEEN ")
	} else {
		r.sb.WriteString(" BETWEEN ")
	}
	if err := r.render(node.Low, next); err != nil {
		return err
	}
	r.sb.WriteString(" AND ")
	if err := r.render(node.High, next); err != nil {
		return err
	}
	r.sb.WriteString(")")
	return nil
}

func (r *filterRenderer) call(node *filter.Call, next int) error {
	switch node.Name {
	case "ifnull", "coalesce":
		return r.plainCall("coalesce", node.Args, next)
	case "nullif":
		return r.plainCall("nullif", node.Args, next)
	case "iif":
		if len(node.Args) != 3 {
			return filterNotRenderable("iif")
		}
		r.sb.WriteString("(CASE WHEN ")
		if err := r.render(node.Args[0], next); err != nil {
			return err
		}
		r.sb.WriteString(" THEN ")
		if err := r.render(node.Args[1], next); err != nil {
			return err
		}
		r.sb.WriteString(" ELSE ")
		if err := r.render(node.Args[2], next); err != nil {
			return err
		}
		r.sb.WriteString(" END)")
		return nil
	}
	target, ok := renderedFunctions[node.Name]
	if !ok {
		return filterNotRenderable(node.Name)
	}
	return r.plainCall("pg_catalog."+target, node.Args, next)
}

func (r *filterRenderer) plainCall(name string, args []filter.Node, next int) error {
	r.sb.WriteString(name + "(")
	for i, arg := range args {
		if i > 0 {
			r.sb.WriteString(", ")
		}
		if err := r.render(arg, next); err != nil {
			return err
		}
	}
	r.sb.WriteString(")")
	return nil
}

func (r *filterRenderer) caseExpr(node *filter.Case, next int) error {
	r.sb.WriteString("(CASE")
	if node.Operand != nil {
		r.sb.WriteString(" ")
		if err := r.render(node.Operand, next); err != nil {
			return err
		}
	}
	for _, branch := range node.Branches {
		r.sb.WriteString(" WHEN ")
		if err := r.render(branch.When, next); err != nil {
			return err
		}
		r.sb.WriteString(" THEN ")
		if err := r.render(branch.Then, next); err != nil {
			return err
		}
	}
	if node.Else != nil {
		r.sb.WriteString(" ELSE ")
		if err := r.render(node.Else, next); err != nil {
			return err
		}
	}
	r.sb.WriteString(" END)")
	return nil
}

func filterColumnMap(state tableState) map[string]string {
	columns := map[string]string{"id": "id", "created_at": "created_at"}
	for name, physical := range state.columns {
		columns[name] = physical
	}
	if state.schema.HasOwner {
		columns[schema.OwnerColumn] = schema.OwnerColumn
	}
	return columns
}

func (s *Store) renderSharedFilter(n namespace, expr string, args []any, state tableState, scope *store.RowScope) (string, []any, error) {
	columns := filterColumnMap(state)
	names := make([]string, 0, len(columns))
	for name := range columns {
		names = append(names, name)
	}
	node, err := filter.Parse(expr, filter.Options{Columns: names, Args: args})
	if err != nil {
		return "", nil, fmt.Errorf("%w: filter: %s", store.ErrInvalid, err.Error())
	}
	prefix, source, lead := scopedSourceAt(ident(n.physical, state.physical), scope, 1)
	rendered, bound, err := renderScopedFilter(node, columns, args, len(lead)+1)
	if err != nil {
		return "", nil, err
	}
	return prefix + "SELECT id FROM " + source + " WHERE " + rendered + " ORDER BY id",
		append(append([]any{}, lead...), bound...), nil
}
