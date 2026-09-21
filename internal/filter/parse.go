package filter

import (
	"strings"
)

const (
	precedenceLowest = iota
	precedenceOr
	precedenceAnd
	precedenceNot
	precedenceEquality
	precedenceRelational
	precedenceAdditive
	precedenceMultiplicative
	precedenceConcat
	precedenceUnary
)

var infixWords = map[string]bool{
	"and": true, "or": true, "is": true, "in": true, "like": true, "between": true,
	"then": true, "else": true, "end": true, "when": true, "escape": true, "distinct": true,
}

type parser struct {
	lex       lexer
	tok       token
	columns   map[string]bool
	args      []any
	params    int
	timeDepth int
}

func (p *parser) advance() error {
	tok, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = tok
	return nil
}

func (p *parser) expect(op string, what string) error {
	if !p.tok.is(op) {
		return errAt(p.tok.pos, "expected %s but found %s", what, p.tok.describe())
	}
	return p.advance()
}

func (p *parser) expression(minPrecedence int) error {
	if err := p.prefix(); err != nil {
		return err
	}
	return p.infix(minPrecedence)
}

func (p *parser) infix(minPrecedence int) error {
	for {
		precedence, consume, err := p.peekOperator()
		if err != nil {
			return err
		}
		if precedence == 0 || precedence <= minPrecedence {
			return nil
		}
		if err := consume(precedence); err != nil {
			return err
		}
	}
}

func (p *parser) peekOperator() (int, func(int) error, error) {
	if p.tok.kind == tokenOp {
		switch p.tok.text {
		case "=", "==", "!=", "<>":
			return precedenceEquality, p.binary, nil
		case "<", "<=", ">", ">=":
			return precedenceRelational, p.binary, nil
		case "+", "-":
			return precedenceAdditive, p.binary, nil
		case "*", "/", "%":
			return precedenceMultiplicative, p.binary, nil
		case "||":
			return precedenceConcat, p.binary, nil
		case "&", "|", "<<", ">>":
			return 0, nil, errAt(p.tok.pos, "%q is a bit operator, which a filter expression may not use", p.tok.text)
		case "->", "->>":
			return 0, nil, errAt(p.tok.pos, "%q reads json, which a filter expression may not do", p.tok.text)
		case ".":
			return 0, nil, errAt(p.tok.pos, "a filter expression may not name a table; write the column on its own")
		}
		return 0, nil, nil
	}
	if p.tok.kind != tokenIdent {
		return 0, nil, nil
	}
	word := strings.ToLower(p.tok.text)
	if reason, bad := rejectedKeywords[word]; bad {
		return 0, nil, errAt(p.tok.pos, "%s", reason)
	}
	switch word {
	case "or":
		return precedenceOr, p.binary, nil
	case "and":
		return precedenceAnd, p.binary, nil
	case "is":
		return precedenceEquality, p.isOperator, nil
	case "in":
		return precedenceEquality, p.inList, nil
	case "like":
		return precedenceEquality, p.like, nil
	case "between":
		return precedenceEquality, p.between, nil
	case "not":
		return precedenceEquality, p.negatedOperator, nil
	}
	return 0, nil, nil
}

func (p *parser) binary(precedence int) error {
	if err := p.advance(); err != nil {
		return err
	}
	return p.expression(precedence)
}

func (p *parser) isOperator(precedence int) error {
	if err := p.advance(); err != nil {
		return err
	}
	if p.tok.keyword("not") {
		if err := p.advance(); err != nil {
			return err
		}
	}
	if p.tok.keyword("distinct") {
		return errAt(p.tok.pos, "is distinct from is not accepted in a filter expression; write is or is not")
	}
	return p.expression(precedence)
}

func (p *parser) negatedOperator(precedence int) error {
	pos := p.tok.pos
	if err := p.advance(); err != nil {
		return err
	}
	switch {
	case p.tok.keyword("in"):
		return p.inList(precedence)
	case p.tok.keyword("like"):
		return p.like(precedence)
	case p.tok.keyword("between"):
		return p.between(precedence)
	case p.tok.keyword("null"):
		return errAt(pos, "not null is not accepted after a value in a filter expression; write is not null")
	}
	return errAt(pos, "not may negate in, like or between here, but %s follows it", p.tok.describe())
}

func (p *parser) like(precedence int) error {
	if err := p.advance(); err != nil {
		return err
	}
	if err := p.expression(precedence); err != nil {
		return err
	}
	if p.tok.keyword("escape") {
		if err := p.advance(); err != nil {
			return err
		}
		if !p.literalToken() {
			return errAt(p.tok.pos, "the escape of a like must be a one-character text literal or a ? argument, not %s", p.tok.describe())
		}
		return p.advance()
	}
	return nil
}

func (p *parser) between(precedence int) error {
	if err := p.advance(); err != nil {
		return err
	}
	if err := p.expression(precedenceAnd); err != nil {
		return err
	}
	if !p.tok.keyword("and") {
		return errAt(p.tok.pos, "between needs an and before its upper bound, but %s follows", p.tok.describe())
	}
	if err := p.advance(); err != nil {
		return err
	}
	return p.expression(precedenceAnd)
}

func (p *parser) inList(precedence int) error {
	if err := p.advance(); err != nil {
		return err
	}
	if !p.tok.is("(") {
		return errAt(p.tok.pos, "in must be followed by a parenthesised list of values in a filter expression, not %s", p.tok.describe())
	}
	if err := p.advance(); err != nil {
		return err
	}
	if p.tok.is(")") {
		return p.advance()
	}
	for {
		if p.tok.keyword("select") {
			return errAt(p.tok.pos, "a filter expression may not read rows itself; it sees only the row it is applied to")
		}
		if err := p.listValue(); err != nil {
			return err
		}
		if p.tok.is(",") {
			if err := p.advance(); err != nil {
				return err
			}
			continue
		}
		break
	}
	return p.expect(")", "a closing parenthesis after the list of values")
}

func (p *parser) listValue() error {
	negated := false
	if p.tok.is("-") || p.tok.is("+") {
		negated = true
		if err := p.advance(); err != nil {
			return err
		}
	}
	if !p.literalToken() {
		return errAt(p.tok.pos, "in accepts only literal values and ? arguments in a filter expression, not %s", p.tok.describe())
	}
	if negated && p.tok.kind != tokenNumber {
		return errAt(p.tok.pos, "a sign may only precede a number in the list of values")
	}
	return p.consumeLiteral()
}

func (p *parser) literalToken() bool {
	switch p.tok.kind {
	case tokenString, tokenNumber, tokenBlob, tokenParam:
		return true
	case tokenIdent:
		word := strings.ToLower(p.tok.text)
		return word == "null" || word == "true" || word == "false"
	}
	return false
}

func (p *parser) consumeLiteral() error {
	if p.timeDepth > 0 {
		switch p.tok.kind {
		case tokenString:
			if err := rejectClockWord(p.tok.text, p.tok.pos); err != nil {
				return err
			}
		case tokenParam:
			if p.params < len(p.args) {
				if text, ok := p.args[p.params].(string); ok {
					if err := rejectClockWord(text, p.tok.pos); err != nil {
						return err
					}
				}
			}
		}
	}
	if p.tok.kind == tokenParam {
		p.params++
	}
	return p.advance()
}

func (p *parser) prefix() error {
	switch {
	case p.tok.is("-"), p.tok.is("+"):
		if err := p.advance(); err != nil {
			return err
		}
		return p.expression(precedenceUnary)
	case p.tok.is("~"):
		return errAt(p.tok.pos, "~ is a bit operator, which a filter expression may not use")
	case p.tok.is("("):
		if err := p.advance(); err != nil {
			return err
		}
		if p.tok.keyword("select") {
			return errAt(p.tok.pos, "a filter expression may not read rows itself; it sees only the row it is applied to")
		}
		if err := p.expression(precedenceLowest); err != nil {
			return err
		}
		return p.expect(")", "a closing parenthesis")
	case p.tok.keyword("not"):
		if err := p.advance(); err != nil {
			return err
		}
		return p.expression(precedenceNot)
	case p.tok.keyword("case"):
		return p.caseExpression()
	case p.tok.kind == tokenParam:
		return p.consumeLiteral()
	case p.tok.kind == tokenString, p.tok.kind == tokenNumber, p.tok.kind == tokenBlob:
		return p.consumeLiteral()
	case p.tok.kind == tokenQuotedIdent:
		return p.column(p.tok.text, p.tok.pos)
	case p.tok.kind == tokenIdent:
		return p.identifier()
	}
	return errAt(p.tok.pos, "a filter expression cannot start a value with %s", p.tok.describe())
}

func (p *parser) identifier() error {
	word := strings.ToLower(p.tok.text)
	if reason, bad := rejectedKeywords[word]; bad {
		return errAt(p.tok.pos, "%s", reason)
	}
	switch word {
	case "null", "true", "false":
		return p.advance()
	case "case":
		return p.caseExpression()
	}
	if infixWords[word] {
		return errAt(p.tok.pos, "a filter expression cannot start a value with %s", p.tok.describe())
	}
	name, pos := p.tok.text, p.tok.pos
	if err := p.advance(); err != nil {
		return err
	}
	if p.tok.is("(") {
		return p.call(word, pos)
	}
	return p.namedColumn(name, pos)
}

func (p *parser) column(name string, pos int) error {
	if err := p.advance(); err != nil {
		return err
	}
	return p.namedColumn(name, pos)
}

func (p *parser) namedColumn(name string, pos int) error {
	if p.tok.is(".") {
		return errAt(pos, "a filter expression may not name a table; write the column on its own")
	}
	if !p.columns[strings.ToLower(name)] {
		return p.unknownColumn(name, pos)
	}
	return nil
}

func (p *parser) call(name string, pos int) error {
	limits, ok := allowedFunctions[name]
	if !ok {
		return errAt(pos, "%s is not one of the functions a filter expression may use (%s)", name, functionList())
	}
	if err := p.advance(); err != nil {
		return err
	}
	if p.tok.is("*") {
		return errAt(p.tok.pos, "%s(*) counts rows, which a filter expression may not do", name)
	}
	if p.tok.keyword("distinct") {
		return errAt(p.tok.pos, "%s cannot take distinct in a filter expression", name)
	}
	if timeFunctions[name] {
		p.timeDepth++
		defer func() { p.timeDepth-- }()
	}
	count := 0
	for !p.tok.is(")") {
		if count > 0 {
			if err := p.expect(",", "a comma between arguments"); err != nil {
				return err
			}
		}
		if err := p.expression(precedenceLowest); err != nil {
			return err
		}
		count++
		if p.tok.kind == tokenEOF {
			return errAt(p.tok.pos, "the arguments of %s are never closed", name)
		}
	}
	if err := p.advance(); err != nil {
		return err
	}
	if count < limits.min || (limits.max >= 0 && count > limits.max) {
		return errAt(pos, "%s takes %s in a filter expression, but %d were given", name, limits.describe(), count)
	}
	if p.tok.keyword("filter") || p.tok.keyword("over") {
		return errAt(p.tok.pos, "%s cannot take a %s clause in a filter expression", name, strings.ToLower(p.tok.text))
	}
	return nil
}

func rejectClockWord(text string, pos int) error {
	word := strings.ToLower(strings.TrimSpace(text))
	if reason, bad := rejectedTimeWords[word]; bad {
		return errAt(pos, "%q %s, so the same filter would mean different things at different moments; compute the moment you mean and bind it as a ? argument", word, reason)
	}
	return nil
}

func (p *parser) caseExpression() error {
	if err := p.advance(); err != nil {
		return err
	}
	if !p.tok.keyword("when") {
		if err := p.expression(precedenceLowest); err != nil {
			return err
		}
	}
	if !p.tok.keyword("when") {
		return errAt(p.tok.pos, "case needs at least one when in a filter expression, but %s follows", p.tok.describe())
	}
	for p.tok.keyword("when") {
		if err := p.advance(); err != nil {
			return err
		}
		if err := p.expression(precedenceLowest); err != nil {
			return err
		}
		if !p.tok.keyword("then") {
			return errAt(p.tok.pos, "each when of a case needs a then, but %s follows", p.tok.describe())
		}
		if err := p.advance(); err != nil {
			return err
		}
		if err := p.expression(precedenceLowest); err != nil {
			return err
		}
	}
	if p.tok.keyword("else") {
		if err := p.advance(); err != nil {
			return err
		}
		if err := p.expression(precedenceLowest); err != nil {
			return err
		}
	}
	if !p.tok.keyword("end") {
		return errAt(p.tok.pos, "case needs an end in a filter expression, but %s follows", p.tok.describe())
	}
	return p.advance()
}

func (r argRange) describe() string {
	switch {
	case r.max < 0 && r.min == 1:
		return "at least one argument"
	case r.max < 0:
		return plural(r.min) + " or more"
	case r.min == r.max:
		return plural(r.min)
	}
	return plural(r.min) + " or " + plural(r.max)
}

func plural(n int) string {
	if n == 1 {
		return "1 argument"
	}
	return itoa(n) + " arguments"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func functionList() string {
	names := make([]string, 0, len(allowedFunctions))
	for name := range allowedFunctions {
		names = append(names, name)
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
