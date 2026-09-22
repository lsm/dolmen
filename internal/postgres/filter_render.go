package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/lsm/dolmen/internal/filter"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type filterRenderer struct {
	columns    map[string]string
	types      map[string]schema.FieldType
	args       []any
	bound      []any
	sb         strings.Builder
	rawBoolean bool
}

var renderedFunctions = map[string]string{
	"abs": "abs", "round": "round", "length": "length", "lower": "lower", "upper": "upper",
	"substr": "substr", "trim": "btrim", "ltrim": "ltrim", "rtrim": "rtrim", "replace": "replace",
	"instr": "strpos",
}

func filterNotRenderable(name string) error {
	return store.NewBackendQueryError(fmt.Sprintf("%s is in the filter language this server accepts, but this storage engine cannot evaluate it yet; rewrite the filter without it, or compute the value and bind it as a ? argument", name), nil)
}

func renderScopedFilter(node filter.Node, columns map[string]string, types map[string]schema.FieldType, args []any, next int) (string, []any, error) {
	r := &filterRenderer{columns: columns, types: types, args: args}
	rendered, err := r.truth(node, next)
	if err != nil {
		return "", nil, err
	}
	return rendered, r.bound, nil
}

func (r *filterRenderer) isBooleanNode(n filter.Node) bool {
	switch node := n.(type) {
	case *filter.Paren:
		return r.isBooleanNode(node.Inner)
	case *filter.Column:
		return r.types[node.Name] == schema.Boolean
	case *filter.Param:
		if node.Index >= 0 && node.Index < len(r.args) {
			_, ok := r.args[node.Index].(bool)
			return ok
		}
	}
	return false
}

func (r *filterRenderer) captureBoolean(n filter.Node, next int) (string, error) {
	saved := r.rawBoolean
	r.rawBoolean = true
	out, err := r.capture(n, next)
	r.rawBoolean = saved
	return out, err
}

func (r *filterRenderer) booleanShaped(n filter.Node) bool {
	return r.isBooleanNode(n) || rendersAsTruthValue(n)
}

func (r *filterRenderer) truth(n filter.Node, next int) (string, error) {
	if r.isBooleanNode(n) {
		return r.captureBoolean(n, next)
	}
	if rendersAsTruthValue(n) {
		return r.capture(n, next)
	}
	if r.affinityOf(n) == affText {
		num, err := r.captureNumeric(n, next)
		if err != nil {
			return "", err
		}
		return "(" + num + " <> 0)", nil
	}
	out, err := r.capture(n, next)
	if err != nil {
		return "", err
	}
	if r.affinityOf(n) == affBlob {
		return "(" + sqliteNumberOfText("convert_from("+out+", 'LATIN1')") + " <> 0)", nil
	}
	return "(" + out + " <> 0)", nil
}

func (r *filterRenderer) placeholder(next int) int { return next + len(r.bound) }

func (r *filterRenderer) render(n filter.Node, next int) error {
	switch node := n.(type) {
	case *filter.Column:
		physical, ok := r.columns[node.Name]
		if !ok {
			physical = node.Name
		}
		if r.types[node.Name] == schema.Boolean && !r.rawBoolean {
			r.sb.WriteString("(" + ident(physical) + ")::int")
			return nil
		}
		r.sb.WriteString(ident(physical))
		return nil
	case *filter.Param:
		if node.Index >= len(r.args) {
			return fmt.Errorf("%w: filter argument %d was not supplied", store.ErrInvalid, node.Index+1)
		}
		r.sb.WriteString("$" + fmt.Sprint(r.placeholder(next)))
		if _, isBool := r.args[node.Index].(bool); isBool && !r.rawBoolean {
			r.sb.WriteString("::int")
		}
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
			inner, err := r.truth(node.Operand, next)
			if err != nil {
				return err
			}
			r.sb.WriteString("NOT (" + inner + ")")
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
		r.sb.WriteString(dollarQuote(`\x`+strings.ToLower(node.Text)) + "::bytea")
	default:
		r.sb.WriteString(dollarQuote(node.Text))
	}
	return nil
}

func dollarQuote(text string) string {
	tag := "$dolmen$"
	for i := 0; strings.Contains(text, tag); i++ {
		tag = "$dolmen" + strconv.Itoa(i) + "$"
	}
	return tag + text + tag
}

func (r *filterRenderer) number(text string) error {
	lowered := strings.ToLower(text)
	if strings.HasPrefix(lowered, "0x") {
		value, err := strconv.ParseUint(lowered[2:], 16, 64)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return fmt.Errorf("%w: the hexadecimal literal %q does not fit in 64 bits", store.ErrInvalid, text)
			}
			return fmt.Errorf("%w: %q is not a hexadecimal integer", store.ErrInvalid, text)
		}
		r.sb.WriteString(strconv.FormatInt(int64(value), 10))
		return nil
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if math.IsInf(f, 1) || f > math.MaxFloat64 {
			r.sb.WriteString("'Infinity'::numeric")
			return nil
		}
		if math.IsInf(f, -1) || f < -math.MaxFloat64 {
			r.sb.WriteString("'-Infinity'::numeric")
			return nil
		}
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
	switch op {
	case "=", "<>", "<", "<=", ">", ">=":
		return r.comparison(node, op, next)
	case "+", "-", "*", "/", "%":
		return r.arithmetic(node, op, next)
	case "AND", "OR":
		left, err := r.truth(node.Left, next)
		if err != nil {
			return err
		}
		right, err := r.truth(node.Right, next)
		if err != nil {
			return err
		}
		r.sb.WriteString("(" + left + " " + op + " " + right + ")")
		return nil
	}
	if op == "||" {
		left, err := r.concatOperand(node.Left, next)
		if err != nil {
			return err
		}
		right, err := r.concatOperand(node.Right, next)
		if err != nil {
			return err
		}
		r.sb.WriteString("(" + left + " || " + right + ")")
		return nil
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

func (r *filterRenderer) concatOperand(n filter.Node, next int) (string, error) {
	if r.affinityOf(n) == affNumber {
		if r.isBooleanNode(n) {
			out, err := r.capture(n, next)
			if err != nil {
				return "", err
			}
			return "(" + out + ")::text", nil
		}
		text, ok := r.staticNumberAsText(n)
		if !ok {
			return "", filterNotRenderable("concatenation of a number this engine cannot render SQLite's text for")
		}
		return dollarQuote(text), nil
	}
	out, err := r.capture(n, next)
	if err != nil {
		return "", err
	}
	if r.affinityOf(n) == affBlob {
		return "convert_from(" + out + ", 'LATIN1')", nil
	}
	return out, nil
}

func (r *filterRenderer) compareVia(op string, left, right filter.Node, next int) (string, error) {
	saved := r.sb
	r.sb = strings.Builder{}
	err := r.comparison(&filter.Binary{Op: op, Left: left, Right: right}, op, next)
	out := r.sb.String()
	r.sb = saved
	return out, err
}

func (r *filterRenderer) is(node *filter.Is, next int) error {
	equal, err := r.compareVia("=", node.Left, node.Right, next)
	if err != nil {
		return err
	}
	left, err := r.captureGuard(node.Left, next)
	if err != nil {
		return err
	}
	right, err := r.captureGuard(node.Right, next)
	if err != nil {
		return err
	}
	joined := "(COALESCE(" + equal + ", FALSE) OR (" + left + " IS NULL AND " + right + " IS NULL))"
	if node.Negated {
		joined = "(NOT " + joined + ")"
	}
	r.sb.WriteString(joined)
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
	parts := make([]string, 0, len(node.List))
	for _, item := range node.List {
		part, err := r.compareVia("=", node.Left, item, next)
		if err != nil {
			return err
		}
		parts = append(parts, part)
	}
	joined := "(" + strings.Join(parts, " OR ") + ")"
	if node.Negated {
		joined = "(NOT " + joined + ")"
	}
	r.sb.WriteString(joined)
	return nil
}

func (r *filterRenderer) likeOperand(n filter.Node, next int) (string, error) {
	switch r.affinityOf(n) {
	case affNumber:
		if r.isBooleanNode(n) {
			out, err := r.capture(n, next)
			if err != nil {
				return "", err
			}
			return "(" + out + ")::text", nil
		}
		if text, ok := r.staticNumberAsText(n); ok {
			return dollarQuote(text), nil
		}
		return "", filterNotRenderable("LIKE over a number this engine cannot render SQLite's text for")
	case affBlob:
		return "", filterNotRenderable("LIKE over a blob")
	}
	return r.capture(n, next)
}

func (r *filterRenderer) like(node *filter.Like, next int) error {
	subject, err := r.likeOperand(node.Left, next)
	if err != nil {
		return err
	}
	pattern, err := r.likeOperand(node.Pattern, next)
	if err != nil {
		return err
	}
	r.sb.WriteString("(" + asciiFold(subject))
	if node.Negated {
		r.sb.WriteString(" NOT LIKE ")
	} else {
		r.sb.WriteString(" LIKE ")
	}
	r.sb.WriteString(asciiFold(pattern) + ` COLLATE "C"`)
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
	low, err := r.compareVia(">=", node.Value, node.Low, next)
	if err != nil {
		return err
	}
	high, err := r.compareVia("<=", node.Value, node.High, next)
	if err != nil {
		return err
	}
	joined := "(" + low + " AND " + high + ")"
	if node.Negated {
		joined = "(NOT " + joined + ")"
	}
	r.sb.WriteString(joined)
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
		condition, err := r.truth(node.Args[0], next)
		if err != nil {
			return err
		}
		r.sb.WriteString("(CASE WHEN " + condition)
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
	switch node.Name {
	case "lower", "upper":
		if len(node.Args) != 1 {
			return filterNotRenderable(node.Name)
		}
		arg, err := r.capture(node.Args[0], next)
		if err != nil {
			return err
		}
		from, to := asciiUpperSet, asciiLowerSet
		if node.Name == "upper" {
			from, to = asciiLowerSet, asciiUpperSet
		}
		r.sb.WriteString("translate(" + arg + ", '" + from + "', '" + to + "')")
		return nil
	case "abs", "round":
		if len(node.Args) >= 1 && r.affinityOf(node.Args[0]) == affText {
			arg, err := r.captureNumeric(node.Args[0], next)
			if err != nil {
				return err
			}
			rest := make([]string, 0, len(node.Args)-1)
			for _, extra := range node.Args[1:] {
				text, err := r.capture(extra, next)
				if err != nil {
					return err
				}
				rest = append(rest, text)
			}
			r.sb.WriteString("pg_catalog." + node.Name + "(" + strings.Join(append([]string{arg}, rest...), ", ") + ")")
			return nil
		}
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

func (r *filterRenderer) caseCondition(operand, when filter.Node, next int) (string, error) {
	if operand == nil {
		return r.truth(when, next)
	}
	return r.compareVia("=", operand, when, next)
}

func (r *filterRenderer) caseExpr(node *filter.Case, next int) error {
	r.sb.WriteString("(CASE")
	for _, branch := range node.Branches {
		r.sb.WriteString(" WHEN ")
		condition, err := r.caseCondition(node.Operand, branch.When, next)
		if err != nil {
			return err
		}
		r.sb.WriteString(condition)
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

func filterTypeMap(state tableState) map[string]schema.FieldType {
	types := map[string]schema.FieldType{"id": schema.Number, "created_at": schema.Timestamp}
	for _, f := range state.schema.Fields {
		types[f.Name] = f.Type
	}
	if state.schema.HasOwner {
		types[schema.OwnerColumn] = schema.Text
	}
	return types
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
	rendered, bound, err := renderScopedFilter(node, columns, filterTypeMap(state), args, len(lead)+1)
	if err != nil {
		return "", nil, err
	}
	return prefix + "SELECT id FROM " + source + " WHERE " + rendered + " ORDER BY id",
		append(append([]any{}, lead...), bound...), nil
}

type affinity int

const (
	affUnknown affinity = iota
	affNumber
	affText
	affBlob
)

const (
	asciiUpperSet   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	asciiLowerSet   = "abcdefghijklmnopqrstuvwxyz"
	sqliteNumHead   = "^[[:space:]]*([+-]?(?:[0-9]+\\.?[0-9]*|\\.[0-9]+)(?:[eE][+-]?[0-9]+)?)"
	sqliteDoubleMax = "1.7976931348623157e308"
	sqliteIntHead   = "^[[:space:]]*([+-]?[0-9]+)"
	sqliteIntMax    = "9223372036854775807"
	sqliteIntMin    = "-9223372036854775808"
	sqliteNumFull   = "^[[:space:]]*[+-]?(?:[0-9]+\\.?[0-9]*|\\.[0-9]+)(?:[eE][+-]?[0-9]+)?[[:space:]]*$"
)

func saturatingNumeric(text string) string {
	return "(CASE WHEN " + text + " IS NULL THEN NULL" +
		" WHEN NOT pg_input_is_valid(" + text + ", 'numeric')" +
		" THEN (CASE WHEN left(btrim(" + text + "), 1) = '-' THEN '-Infinity' ELSE 'Infinity' END)::numeric" +
		" WHEN " + text + "::numeric > " + sqliteDoubleMax + " THEN 'Infinity'::numeric" +
		" WHEN " + text + "::numeric < -" + sqliteDoubleMax + " THEN '-Infinity'::numeric" +
		" ELSE " + text + "::numeric END)"
}

var sqliteNumeralHead = regexp.MustCompile(`^[\t\n\f\r ]*[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`)

var sqliteNumericText = regexp.MustCompile(`^[\t\n\f\r ]*[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?[\t\n\f\r ]*$`)

func (r *filterRenderer) declaredAffinityOf(n filter.Node) affinity {
	switch node := n.(type) {
	case *filter.Paren:
		return r.declaredAffinityOf(node.Inner)
	case *filter.Column:
		switch r.types[node.Name] {
		case schema.Number, schema.Boolean:
			return affNumber
		case schema.String, schema.Text, schema.Timestamp, schema.JSON:
			return affText
		}
	}
	return affUnknown
}

func (r *filterRenderer) staticTextOf(n filter.Node) (string, bool) {
	switch node := n.(type) {
	case *filter.Paren:
		return r.staticTextOf(node.Inner)
	case *filter.Literal:
		if node.Kind == filter.LiteralString {
			return node.Text, true
		}
	case *filter.Param:
		if node.Index >= 0 && node.Index < len(r.args) {
			if text, ok := r.args[node.Index].(string); ok {
				return text, true
			}
		}
	}
	return "", false
}

func sqliteRealText(f float64) (string, bool) {
	switch {
	case math.IsNaN(f):
		return "", false
	case math.IsInf(f, 1):
		return "Inf", true
	case math.IsInf(f, -1):
		return "-Inf", true
	}
	if f == 0 {
		f = 0
	}
	text := strconv.FormatFloat(f, 'g', 15, 64)
	if back, err := strconv.ParseFloat(text, 64); err != nil || back != f {
		return "", false
	}
	mantissa, exponent, split := strings.Cut(text, "e")
	if !strings.Contains(mantissa, ".") {
		mantissa += ".0"
	}
	if split {
		return mantissa + "e" + exponent, true
	}
	return mantissa, true
}

func sqliteTextOfNumberLiteral(text string) (string, bool) {
	lowered := strings.ToLower(text)
	if strings.HasPrefix(lowered, "0x") {
		value, err := strconv.ParseUint(lowered[2:], 16, 64)
		if err != nil {
			return "", false
		}
		return strconv.FormatInt(int64(value), 10), true
	}
	if !numeralIsReal(text) {
		return text, true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return "", false
	}
	return sqliteRealText(f)
}

func (r *filterRenderer) staticNumberAsText(n filter.Node) (string, bool) {
	switch node := n.(type) {
	case *filter.Paren:
		return r.staticNumberAsText(node.Inner)
	case *filter.Unary:
		if node.Op != "-" && node.Op != "+" {
			return "", false
		}
		inner, ok := r.staticNumberAsText(node.Operand)
		if !ok {
			return "", false
		}
		if node.Op == "+" {
			return inner, true
		}
		f, err := strconv.ParseFloat(inner, 64)
		if err != nil {
			return "", false
		}
		if !strings.ContainsAny(inner, ".eE") {
			if whole, err := strconv.ParseInt("-"+inner, 10, 64); err == nil {
				return strconv.FormatInt(whole, 10), true
			}
			f, err := strconv.ParseFloat("-"+inner, 64)
			if err != nil {
				return "", false
			}
			return sqliteRealText(f)
		}
		return sqliteRealText(-f)
	case *filter.Literal:
		if node.Kind == filter.LiteralNumber {
			return sqliteTextOfNumberLiteral(node.Text)
		}
	case *filter.Param:
		if node.Index < 0 || node.Index >= len(r.args) {
			return "", false
		}
		switch value := r.args[node.Index].(type) {
		case int:
			return strconv.Itoa(value), true
		case int8:
			return strconv.FormatInt(int64(value), 10), true
		case int16:
			return strconv.FormatInt(int64(value), 10), true
		case int32:
			return strconv.FormatInt(int64(value), 10), true
		case int64:
			return strconv.FormatInt(value, 10), true
		case float32:
			return sqliteRealText(float64(value))
		case float64:
			return sqliteRealText(value)
		case json.Number:
			return sqliteTextOfNumberLiteral(value.String())
		}
	}
	return "", false
}

func (r *filterRenderer) affinityOf(n filter.Node) affinity {
	switch node := n.(type) {
	case *filter.Paren:
		return r.affinityOf(node.Inner)
	case *filter.Column:
		switch r.types[node.Name] {
		case schema.Number, schema.Boolean:
			return affNumber
		case schema.String, schema.Text, schema.Timestamp, schema.JSON:
			return affText
		}
		return affUnknown
	case *filter.Param:
		if node.Index < 0 || node.Index >= len(r.args) {
			return affUnknown
		}
		switch r.args[node.Index].(type) {
		case string:
			return affText
		case []byte:
			return affBlob
		case int, int8, int16, int32, int64, float32, float64, json.Number:
			return affNumber
		}
		return affUnknown
	case *filter.Literal:
		switch node.Kind {
		case filter.LiteralNumber:
			return affNumber
		case filter.LiteralString:
			return affText
		case filter.LiteralBlob:
			return affBlob
		}
		return affUnknown
	case *filter.Call:
		switch node.Name {
		case "abs", "round", "length", "instr":
			return affNumber
		case "lower", "upper", "substr", "trim", "ltrim", "rtrim", "replace":
			return affText
		case "iif":
			if len(node.Args) == 3 {
				return r.agreedAffinity(node.Args[1], node.Args[2])
			}
		case "coalesce", "ifnull":
			if len(node.Args) > 0 {
				return r.agreedAffinity(node.Args...)
			}
		case "nullif":
			if len(node.Args) > 0 {
				return r.affinityOf(node.Args[0])
			}
		}
		return affUnknown
	case *filter.Binary:
		if node.Op == "||" {
			return affText
		}
		return affUnknown
	case *filter.Unary:
		if node.Op == "-" || node.Op == "+" {
			return r.affinityOf(node.Operand)
		}
		return affUnknown
	case *filter.Case:
		return r.agreedAffinity(caseResults(node)...)
	}
	return affUnknown
}

func caseResults(node *filter.Case) []filter.Node {
	out := make([]filter.Node, 0, len(node.Branches)+1)
	for _, b := range node.Branches {
		out = append(out, b.Then)
	}
	if node.Else != nil {
		out = append(out, node.Else)
	}
	return out
}

func (r *filterRenderer) agreedAffinity(nodes ...filter.Node) affinity {
	agreed := affUnknown
	for _, n := range nodes {
		if isNullLiteral(n) {
			continue
		}
		a := r.affinityOf(n)
		if a == affUnknown {
			return affUnknown
		}
		if agreed == affUnknown {
			agreed = a
			continue
		}
		if agreed != a {
			return affUnknown
		}
	}
	return agreed
}

func isNullLiteral(n filter.Node) bool {
	switch node := n.(type) {
	case *filter.Paren:
		return isNullLiteral(node.Inner)
	case *filter.Literal:
		return node.Kind == filter.LiteralNull
	}
	return false
}

func comparisonIsAcrossClasses(left, right affinity) bool {
	if left == affUnknown || right == affUnknown || left == right {
		return false
	}
	return true
}

func classRank(a affinity) int {
	switch a {
	case affNumber:
		return 1
	case affText:
		return 2
	case affBlob:
		return 3
	}
	return 0
}

func crossClassAnswer(op string, left, right affinity) (string, bool) {
	lr, rr := classRank(left), classRank(right)
	switch op {
	case "=", "==":
		return "FALSE", true
	case "!=", "<>":
		return "TRUE", true
	case "<":
		return boolLiteral(lr < rr), true
	case "<=":
		return boolLiteral(lr < rr), true
	case ">":
		return boolLiteral(lr > rr), true
	case ">=":
		return boolLiteral(lr > rr), true
	}
	return "", false
}

func boolLiteral(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}

func rendersAsTruthValue(n filter.Node) bool {
	switch node := n.(type) {
	case *filter.Paren:
		return rendersAsTruthValue(node.Inner)
	case *filter.Is, *filter.In, *filter.Like, *filter.Between:
		return true
	case *filter.Unary:
		return strings.EqualFold(node.Op, "not")
	case *filter.Literal:
		return node.Kind == filter.LiteralTrue || node.Kind == filter.LiteralFalse
	case *filter.Binary:
		switch node.Op {
		case "=", "==", "!=", "<>", "<", "<=", ">", ">=", "and", "or", "AND", "OR":
			return true
		}
		return false
	}
	return false
}

func (r *filterRenderer) capture(n filter.Node, next int) (string, error) {
	saved := r.sb
	r.sb = strings.Builder{}
	err := r.render(n, next)
	out := r.sb.String()
	r.sb = saved
	return out, err
}

func (r *filterRenderer) captureNumeric(n filter.Node, next int) (string, error) {
	text, err := r.capture(n, next)
	if err != nil {
		return "", err
	}
	if r.affinityOf(n) == affText {
		return sqliteNumberOfText(text), nil
	}
	return "(" + text + ")::numeric", nil
}

func sqliteNumberOfText(text string) string {
	head := "(substring(" + text + " FROM '" + sqliteNumHead + "'))"
	return "(CASE WHEN " + text + " IS NULL THEN NULL ELSE COALESCE(" + saturatingNumeric(head) + ", 0) END)"
}

func (r *filterRenderer) comparison(node *filter.Binary, op string, next int) error {
	if r.booleanShaped(node.Left) && r.booleanShaped(node.Right) {
		left, err := r.captureBoolean(node.Left, next)
		if err != nil {
			return err
		}
		right, err := r.captureBoolean(node.Right, next)
		if err != nil {
			return err
		}
		r.sb.WriteString("(" + left + " " + op + " " + right + ")")
		return nil
	}
	la, ra := r.declaredAffinityOf(node.Left), r.declaredAffinityOf(node.Right)
	switch {
	case la == affNumber && ra != affNumber:
		return r.numericAffinity(node, op, next, true)
	case ra == affNumber && la != affNumber:
		return r.numericAffinity(node, op, next, false)
	case la == affText && ra == affUnknown:
		return r.textAffinity(node, op, next, true)
	case ra == affText && la == affUnknown:
		return r.textAffinity(node, op, next, false)
	}
	return r.storageClassComparison(node, op, next)
}

func (r *filterRenderer) substitutedComparison(node *filter.Binary, op string, next int, otherIsRight bool, replacement, suffix string) error {
	var left, right string
	var err error
	if otherIsRight {
		if left, err = r.capture(node.Left, next); err != nil {
			return err
		}
		right = replacement
	} else {
		left = replacement
		if right, err = r.capture(node.Right, next); err != nil {
			return err
		}
	}
	r.sb.WriteString("(" + left + " " + op + " " + right + suffix + ")")
	return nil
}

func (r *filterRenderer) textAffinity(node *filter.Binary, op string, next int, otherIsRight bool) error {
	other := node.Left
	if otherIsRight {
		other = node.Right
	}
	if r.affinityOf(other) != affNumber {
		return r.storageClassComparison(node, op, next)
	}
	if text, ok := r.staticNumberAsText(other); ok {
		return r.substitutedComparison(node, op, next, otherIsRight, dollarQuote(text), ` COLLATE "C"`)
	}
	return filterNotRenderable("a text column compared against a computed number")
}

func staticNumericLiteral(text string) string {
	trimmed := strings.TrimSpace(text)
	if whole, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return "(" + strconv.FormatInt(whole, 10) + ")"
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return "(" + trimmed + ")"
	}
	if math.IsInf(f, 1) || f > math.MaxFloat64 {
		return "'Infinity'::numeric"
	}
	if math.IsInf(f, -1) || f < -math.MaxFloat64 {
		return "'-Infinity'::numeric"
	}
	return "(" + strconv.FormatFloat(f, 'g', -1, 64) + ")"
}

func (r *filterRenderer) numericAffinity(node *filter.Binary, op string, next int, otherIsRight bool) error {
	other := node.Left
	if otherIsRight {
		other = node.Right
	}
	if r.affinityOf(other) != affText {
		return r.storageClassComparison(node, op, next)
	}
	if text, ok := r.staticTextOf(other); ok {
		if !sqliteNumericText.MatchString(text) {
			return r.storageClassComparison(node, op, next)
		}
		return r.substitutedComparison(node, op, next, otherIsRight, staticNumericLiteral(text), "")
	}
	return r.runtimeNumericAffinity(node, op, next, otherIsRight)
}

func (r *filterRenderer) runtimeNumericAffinity(node *filter.Binary, op string, next int, otherIsRight bool) error {
	left, err := r.capture(node.Left, next)
	if err != nil {
		return err
	}
	right, err := r.capture(node.Right, next)
	if err != nil {
		return err
	}
	text := left
	if otherIsRight {
		text = right
	}
	converted := saturatingNumeric(left) + " " + op + " " + right
	if otherIsRight {
		converted = left + " " + op + " " + saturatingNumeric(right)
	}
	answer, ok := crossClassAnswer(op, r.affinityOf(node.Left), r.affinityOf(node.Right))
	if !ok {
		return r.storageClassComparison(node, op, next)
	}
	r.sb.WriteString("(CASE WHEN " + text + " ~ '" + sqliteNumFull + "' THEN (" + converted + ")" +
		" ELSE (CASE WHEN " + left + " IS NULL OR " + right + " IS NULL THEN NULL ELSE " + answer + " END) END)")
	return nil
}

func isParamNode(n filter.Node) bool {
	switch node := n.(type) {
	case *filter.Paren:
		return isParamNode(node.Inner)
	case *filter.Param:
		return true
	}
	return false
}

func (r *filterRenderer) captureGuard(n filter.Node, next int) (string, error) {
	out, err := r.capture(n, next)
	if err != nil {
		return "", err
	}
	if !isParamNode(n) {
		return out, nil
	}
	switch r.affinityOf(n) {
	case affNumber:
		return out + "::numeric", nil
	case affText:
		return out + "::text", nil
	case affBlob:
		return out + "::bytea", nil
	}
	return out, nil
}

func (r *filterRenderer) storageClassComparison(node *filter.Binary, op string, next int) error {
	left, right := r.affinityOf(node.Left), r.affinityOf(node.Right)
	if comparisonIsAcrossClasses(left, right) {
		if answer, ok := crossClassAnswer(op, left, right); ok {
			subject, err := r.captureGuard(node.Left, next)
			if err != nil {
				return err
			}
			object, err := r.captureGuard(node.Right, next)
			if err != nil {
				return err
			}
			r.sb.WriteString("(CASE WHEN " + subject + " IS NULL OR " + object + " IS NULL" +
				" THEN NULL ELSE " + answer + " END)")
			return nil
		}
	}
	r.sb.WriteString("(")
	if err := r.render(node.Left, next); err != nil {
		return err
	}
	r.sb.WriteString(" " + op + " ")
	if err := r.render(node.Right, next); err != nil {
		return err
	}
	if left == affText && right == affText {
		r.sb.WriteString(` COLLATE "C"`)
	}
	r.sb.WriteString(")")
	return nil
}

func (r *filterRenderer) moduloOperand(n filter.Node, raw, numeric string) string {
	if r.affinityOf(n) != affText {
		return sqliteInteger(numeric)
	}
	head := "(substring(" + raw + " FROM '" + sqliteIntHead + "'))"
	return "(CASE WHEN " + raw + " IS NULL THEN NULL ELSE " +
		sqliteInteger("COALESCE("+head+"::numeric, 0)") + " END)"
}

func sqliteInteger(text string) string {
	return "(CASE WHEN " + text + " IS NULL THEN NULL ELSE" +
		" trunc(least(greatest(" + text + ", " + sqliteIntMin + "), " + sqliteIntMax + ")) END)"
}

func (r *filterRenderer) staticallyReal(n filter.Node) bool {
	switch node := n.(type) {
	case *filter.Paren:
		return r.staticallyReal(node.Inner)
	case *filter.Unary:
		if node.Op == "-" || node.Op == "+" {
			if inner, ok := node.Operand.(*filter.Literal); ok && inner.Kind == filter.LiteralNumber &&
				!strings.ContainsAny(inner.Text, ".eE") && !strings.HasPrefix(strings.ToLower(inner.Text), "0x") {
				_, err := strconv.ParseInt(node.Op+strings.TrimSpace(inner.Text), 10, 64)
				return err != nil
			}
			return r.staticallyReal(node.Operand)
		}
	case *filter.Literal:
		switch node.Kind {
		case filter.LiteralNumber:
			if strings.HasPrefix(strings.ToLower(node.Text), "0x") {
				return false
			}
			return numeralIsReal(node.Text)
		case filter.LiteralString:
			return numeralHeadIsReal(node.Text)
		}
	case *filter.Param:
		if node.Index < 0 || node.Index >= len(r.args) {
			return false
		}
		switch value := r.args[node.Index].(type) {
		case float32, float64:
			return true
		case json.Number:
			return numeralIsReal(value.String())
		case string:
			return numeralHeadIsReal(value)
		}
	}
	return false
}

func numeralIsReal(text string) bool {
	if strings.ContainsAny(text, ".eE") {
		return true
	}
	_, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	return err != nil
}

func numeralHeadIsReal(text string) bool {
	head := sqliteNumeralHead.FindString(text)
	if head == "" {
		return false
	}
	return numeralIsReal(head)
}

func (r *filterRenderer) integerClassed(n filter.Node, raw, numeric string) string {
	if r.staticallyReal(n) {
		return "FALSE"
	}
	within := numeric + " BETWEEN " + sqliteIntMin + " AND " + sqliteIntMax
	if r.affinityOf(n) == affText {
		return "(COALESCE((substring(" + raw + " FROM '" + sqliteNumHead + "')), '') !~ '[.eE]' AND " + within + ")"
	}
	return "(" + numeric + " = trunc(" + numeric + ") AND " + within + ")"
}

func (r *filterRenderer) numericOperand(n filter.Node, next int) (string, string, error) {
	raw, err := r.capture(n, next)
	if err != nil {
		return "", "", err
	}
	if r.affinityOf(n) == affText {
		return raw, sqliteNumberOfText(raw), nil
	}
	return raw, "(" + raw + ")::numeric", nil
}

func (r *filterRenderer) arithmetic(node *filter.Binary, op string, next int) error {
	rawLeft, left, err := r.numericOperand(node.Left, next)
	if err != nil {
		return err
	}
	rawRight, right, err := r.numericOperand(node.Right, next)
	if err != nil {
		return err
	}
	leftClass := r.integerClassed(node.Left, rawLeft, left)
	rightClass := r.integerClassed(node.Right, rawRight, right)
	bothInteger := leftClass != "FALSE" && rightClass != "FALSE"
	switch op {
	case "/":
		exact := "NULLIF(" + left + " / NULLIF(" + right + ", 0), 'NaN'::numeric)"
		real := doubleQuotient(left, right)
		if !bothInteger {
			r.sb.WriteString("(" + real + ")")
			return nil
		}
		r.sb.WriteString("(CASE WHEN " + leftClass + " AND " + rightClass +
			" THEN trunc(" + exact + ") ELSE " + real + " END)")
	case "%":
		r.sb.WriteString("(" + r.moduloOperand(node.Left, rawLeft, left) +
			" % NULLIF(" + r.moduloOperand(node.Right, rawRight, right) + ", 0))")
	default:
		exact := "(" + left + " " + op + " " + right + ")"
		real := doubleArithmetic(left, op, right)
		if !bothInteger {
			r.sb.WriteString(real)
			return nil
		}
		r.sb.WriteString("(CASE WHEN " + leftClass + " AND " + rightClass +
			" THEN " + exact + " ELSE " + real + " END)")
	}
	return nil
}

func doubleArithmetic(left, op, right string) string {
	return withoutNaN("((" + left + ")::float8 " + op + " (" + right + ")::float8)::text::numeric")
}

func doubleQuotient(left, right string) string {
	return withoutNaN("((" + left + ")::float8 / NULLIF((" + right + ")::float8, 0))::text::numeric")
}

func withoutNaN(text string) string {
	return "NULLIF(" + text + ", 'NaN'::numeric)"
}

func asciiFold(expr string) string {
	return "translate(" + expr + ", '" + asciiUpperSet + "', '" + asciiLowerSet + "')"
}
