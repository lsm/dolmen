package store

import (
	"strings"
	"unicode"

	"github.com/lsm/dolmen/internal/schema"
)

// validateQueryTables checks that a raw SELECT/WITH statement does not
// reference internal or reserved tables. It walks the statement without
// executing it, so it cannot be bypassed by quoting or string literals.
func validateQueryTables(stmt string) error {
	s := newQueryScanner(stmt)
	if err := s.parseStatement(); err != nil {
		return err
	}
	return nil
}

// queryScanner is a small SQL tokenizer/parser used only to locate table
// references in SELECT/WITH statements.
type queryScanner struct {
	s   string
	i   int
	buf *token
}

type token struct {
	typ string
	val string
}

func newQueryScanner(stmt string) *queryScanner {
	return &queryScanner{s: stmt}
}

func (s *queryScanner) next() (token, error) {
	if s.buf != nil {
		t := *s.buf
		s.buf = nil
		return t, nil
	}
	return s.scanToken()
}

func (s *queryScanner) peek() (token, error) {
	if s.buf != nil {
		return *s.buf, nil
	}
	t, err := s.scanToken()
	if err != nil {
		return token{}, err
	}
	s.buf = &t
	return t, nil
}

func (s *queryScanner) expect(kw string) error {
	t, err := s.next()
	if err != nil {
		return err
	}
	if strings.EqualFold(t.val, kw) {
		return nil
	}
	return invalidf("unexpected token %q, expected %q", t.val, kw)
}

func (s *queryScanner) scanToken() (token, error) {
	for {
		s.skipWhitespace()
		if s.i >= len(s.s) {
			return token{typ: "eof", val: ""}, nil
		}

		c := s.s[s.i]
		switch c {
		case '-':
			if s.i+1 < len(s.s) && s.s[s.i+1] == '-' {
				s.skipLineComment()
				continue
			}
			s.i++
			return token{typ: "op", val: "-"}, nil
		case '/':
			if s.i+1 < len(s.s) && s.s[s.i+1] == '*' {
				s.skipBlockComment()
				continue
			}
			s.i++
			return token{typ: "op", val: "/"}, nil
		case '\'':
			v, err := s.readSingleQuoted()
			if err != nil {
				return token{}, err
			}
			return token{typ: "string", val: v}, nil
		case '"':
			v, err := s.readQuoted(c)
			if err != nil {
				return token{}, err
			}
			return token{typ: "ident", val: v}, nil
		case '`':
			v, err := s.readQuoted(c)
			if err != nil {
				return token{}, err
			}
			return token{typ: "ident", val: v}, nil
		case '[':
			v, err := s.readBracketed()
			if err != nil {
				return token{}, err
			}
			return token{typ: "ident", val: v}, nil
		case '(':
			s.i++
			return token{typ: "punct", val: "("}, nil
		case ')':
			s.i++
			return token{typ: "punct", val: ")"}, nil
		case ',':
			s.i++
			return token{typ: "punct", val: ","}, nil
		case ';':
			s.i++
			return token{typ: "punct", val: ";"}, nil
		case '.':
			s.i++
			return token{typ: "punct", val: "."}, nil
		default:
			if isIdentStart(c) {
				return token{typ: "ident", val: s.readIdent()}, nil
			}
			if unicode.IsDigit(rune(c)) || (c == '.' && s.i+1 < len(s.s) && unicode.IsDigit(rune(s.s[s.i+1]))) {
				return token{typ: "number", val: s.readNumber()}, nil
			}
			s.i++
			return token{typ: "op", val: string(c)}, nil
		}
	}
}

func (s *queryScanner) skipWhitespace() {
	for s.i < len(s.s) {
		c := s.s[s.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			s.i++
			continue
		}
		break
	}
}

func (s *queryScanner) skipLineComment() {
	s.i += 2 // --
	for s.i < len(s.s) && s.s[s.i] != '\n' {
		s.i++
	}
}

func (s *queryScanner) skipBlockComment() {
	s.i += 2 // /*
	for s.i < len(s.s) {
		if s.i+1 < len(s.s) && s.s[s.i] == '*' && s.s[s.i+1] == '/' {
			s.i += 2
			return
		}
		s.i++
	}
}

func (s *queryScanner) readSingleQuoted() (string, error) {
	start := s.i
	s.i++ // '
	var b strings.Builder
	b.WriteByte('\'')
	for s.i < len(s.s) {
		c := s.s[s.i]
		if c == '\'' {
			if s.i+1 < len(s.s) && s.s[s.i+1] == '\'' {
				b.WriteByte('\'')
				s.i += 2
				continue
			}
			b.WriteByte('\'')
			s.i++
			return b.String(), nil
		}
		b.WriteByte(c)
		s.i++
	}
	return "", invalidf("unterminated string literal at %d", start)
}

func (s *queryScanner) readQuoted(quote byte) (string, error) {
	start := s.i
	s.i++ // quote
	var b strings.Builder
	b.WriteByte(quote)
	for s.i < len(s.s) {
		c := s.s[s.i]
		if c == quote {
			if s.i+1 < len(s.s) && s.s[s.i+1] == quote {
				b.WriteByte(quote)
				s.i += 2
				continue
			}
			b.WriteByte(quote)
			s.i++
			return b.String(), nil
		}
		b.WriteByte(c)
		s.i++
	}
	return "", invalidf("unterminated quoted identifier at %d", start)
}

func (s *queryScanner) readBracketed() (string, error) {
	start := s.i
	s.i++ // [
	var b strings.Builder
	b.WriteByte('[')
	for s.i < len(s.s) {
		if s.i+1 < len(s.s) && s.s[s.i] == ']' && s.s[s.i+1] == ']' {
			b.WriteByte(']')
			s.i += 2
			continue
		}
		if s.s[s.i] == ']' {
			b.WriteByte(']')
			s.i++
			return b.String(), nil
		}
		b.WriteByte(s.s[s.i])
		s.i++
	}
	return "", invalidf("unterminated bracketed identifier at %d", start)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || unicode.IsDigit(rune(c))
}

func (s *queryScanner) readIdent() string {
	start := s.i
	for s.i < len(s.s) && isIdentCont(s.s[s.i]) {
		s.i++
	}
	return s.s[start:s.i]
}

func (s *queryScanner) readNumber() string {
	start := s.i
	prev := byte(0)
	for s.i < len(s.s) {
		c := s.s[s.i]
		switch {
		case unicode.IsDigit(rune(c)):
		case c == '.':
		case c == 'e' || c == 'E':
		case (c == '+' || c == '-') && (prev == 'e' || prev == 'E'):
		default:
			return s.s[start:s.i]
		}
		prev = c
		s.i++
	}
	return s.s[start:s.i]
}

func isQuotedIdent(v string) bool {
	if v == "" {
		return false
	}
	c := v[0]
	return c == '"' || c == '`' || c == '['
}

func unquoteIdent(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '"':
		if len(v) >= 2 && v[len(v)-1] == '"' {
			inner := v[1 : len(v)-1]
			return strings.ReplaceAll(inner, "\"\"", "\"")
		}
	case '`':
		if len(v) >= 2 && v[len(v)-1] == '`' {
			inner := v[1 : len(v)-1]
			return strings.ReplaceAll(inner, "``", "`")
		}
	case '[':
		if len(v) >= 2 && v[len(v)-1] == ']' {
			inner := v[1 : len(v)-1]
			return strings.ReplaceAll(inner, "]]", "]")
		}
	}
	return v
}

func isKeyword(t token, kw string) bool {
	return t.typ == "ident" && !isQuotedIdent(t.val) && strings.EqualFold(t.val, kw)
}

func unquoteString(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

func isPragmaFunction(rawName string) bool {
	return strings.HasPrefix(strings.ToLower(unquoteIdent(rawName)), "pragma_")
}

func (s *queryScanner) parsePragmaArgs(schema, rawName string) error {
	if err := s.expect("("); err != nil {
		return err
	}
	arg, err := s.next()
	if err != nil {
		return err
	}
	if arg.typ != "string" {
		return invalidf("pragma argument must be a single string literal")
	}
	if err := s.expect(")"); err != nil {
		return err
	}
	name := unquoteIdent(rawName)
	if schema != "" {
		name = unquoteIdent(schema) + "." + name
	}
	table := unquoteString(arg.val)
	if i := strings.LastIndex(table, "."); i >= 0 {
		table = table[i+1:]
	}
	if !isUserTable(table) {
		return invalidf("query references reserved table %q via %s", table, name)
	}
	return nil
}

var selectStop = map[string]bool{
	")":         true,
	"union":     true,
	"intersect": true,
	"except":    true,
}

var fromStop = map[string]bool{
	")":         true,
	"from":      true,
	"union":     true,
	"intersect": true,
	"except":    true,
}

var joinConditionStop = map[string]bool{
	")":         true,
	",":         true,
	"where":     true,
	"group":     true,
	"having":    true,
	"order":     true,
	"limit":     true,
	"union":     true,
	"intersect": true,
	"except":    true,
	"inner":     true,
	"cross":     true,
	"left":      true,
	"right":     true,
	"full":      true,
	"outer":     true,
	"natural":   true,
	"join":      true,
}

var clauseEndStop = map[string]bool{
	"where":     true,
	"group":     true,
	"having":    true,
	"order":     true,
	"limit":     true,
	"window":    true,
	"union":     true,
	"intersect": true,
	"except":    true,
	")":         true,
	";":         true,
}

func isStopToken(t token, stop map[string]bool) bool {
	if t.typ == "eof" {
		return true
	}
	if t.typ == "punct" {
		return stop[t.val]
	}
	return t.typ == "ident" && !isQuotedIdent(t.val) && stop[strings.ToLower(t.val)]
}

func isClauseEnd(t token) bool {
	if t.typ == "eof" {
		return true
	}
	if t.typ == "punct" {
		return t.val == ")" || t.val == ";"
	}
	return t.typ == "ident" && !isQuotedIdent(t.val) && clauseEndStop[strings.ToLower(t.val)]
}

func isJoinOp(t token) bool {
	if t.typ != "ident" || isQuotedIdent(t.val) {
		return false
	}
	switch strings.ToLower(t.val) {
	case "inner", "cross", "left", "right", "full", "outer", "natural", "join":
		return true
	}
	return false
}

func (s *queryScanner) parseStatement() error {
	t, err := s.peek()
	if err != nil {
		return err
	}
	if isKeyword(t, "with") {
		if err := s.parseWith(); err != nil {
			return err
		}
	} else if !isKeyword(t, "select") && !isKeyword(t, "values") {
		return invalidf("only SELECT/WITH statements are allowed")
	}
	return s.parseSelectStatement()
}

func (s *queryScanner) parseWith() error {
	if err := s.expect("with"); err != nil {
		return err
	}
	if t, _ := s.peek(); isKeyword(t, "recursive") {
		s.next()
	}
	for {
		t, err := s.next()
		if err != nil {
			return err
		}
		if t.typ != "ident" {
			return invalidf("expected CTE name, got %q", t.val)
		}
		if t2, _ := s.peek(); t2.val == "(" {
			if err := s.scanParenthesized(); err != nil {
				return err
			}
		}
		if err := s.expect("as"); err != nil {
			return err
		}
		if t, _ := s.peek(); isKeyword(t, "not") {
			s.next()
			if err := s.expect("materialized"); err != nil {
				return err
			}
		} else if isKeyword(t, "materialized") {
			s.next()
		}
		if err := s.expect("("); err != nil {
			return err
		}
		if err := s.parseStatement(); err != nil {
			return err
		}
		if err := s.expect(")"); err != nil {
			return err
		}
		if t2, _ := s.peek(); t2.val == "," {
			s.next()
			continue
		}
		return nil
	}
}

func (s *queryScanner) parseSelectStatement() error {
	if err := s.parseCore(); err != nil {
		return err
	}
	for {
		t, err := s.peek()
		if err != nil {
			return err
		}
		if !isKeyword(t, "union") && !isKeyword(t, "intersect") && !isKeyword(t, "except") {
			return nil
		}
		s.next()
		if isKeyword(t, "union") {
			if t2, _ := s.peek(); isKeyword(t2, "all") {
				s.next()
			}
		}
		if err := s.parseCore(); err != nil {
			return err
		}
	}
}

func (s *queryScanner) parseCore() error {
	t, _ := s.peek()
	if isKeyword(t, "select") {
		return s.parseSelectCore()
	}
	if isKeyword(t, "values") {
		return s.parseValuesCore()
	}
	return invalidf("expected SELECT or VALUES")
}

func (s *queryScanner) parseValuesCore() error {
	if err := s.expect("values"); err != nil {
		return err
	}
	for {
		if err := s.scanParenthesized(); err != nil {
			return err
		}
		t, _ := s.peek()
		if t.val == "," {
			s.next()
			continue
		}
		break
	}
	_, err := s.scanUntil(selectStop)
	return err
}

func (s *queryScanner) parseSelectCore() error {
	if err := s.expect("select"); err != nil {
		return err
	}

	// Skip the SELECT list. Stop when we reach a top-level FROM.
	t, err := s.scanUntil(fromStop)
	if err != nil {
		return err
	}
	if isKeyword(t, "from") {
		s.next() // consume FROM
		if err := s.parseTableList(); err != nil {
			return err
		}
	}

	// Scan the rest of the core (WHERE, GROUP BY, ORDER BY, LIMIT) looking for
	// subqueries that may reference reserved tables.
	_, err = s.scanUntil(selectStop)
	return err
}

// scanUntil consumes tokens until it hits a stop token at the top level of the
// current scope. Parenthesized groups are recursively scanned so that
// subqueries inside expressions are checked. It returns the stop token without
// consuming it.
func (s *queryScanner) scanUntil(stop map[string]bool) (token, error) {
	for {
		t, err := s.peek()
		if err != nil {
			return token{}, err
		}
		if isStopToken(t, stop) {
			return t, nil
		}
		if t.val == "(" {
			if err := s.scanParenthesized(); err != nil {
				return token{}, err
			}
			continue
		}
		if _, err := s.next(); err != nil {
			return token{}, err
		}
	}
}

func (s *queryScanner) scanParenthesized() error {
	if err := s.expect("("); err != nil {
		return err
	}
	t, err := s.peek()
	if err != nil {
		return err
	}
	if isKeyword(t, "select") || isKeyword(t, "with") {
		if err := s.parseStatement(); err != nil {
			return err
		}
	} else {
		if _, err := s.scanUntil(map[string]bool{")": true}); err != nil {
			return err
		}
	}
	return s.expect(")")
}

func (s *queryScanner) parseTableList() error {
	for {
		t, err := s.peek()
		if err != nil {
			return err
		}

		switch {
		case t.val == ",":
			s.next()
			continue
		case isJoinOp(t):
			if err := s.consumeJoinOp(); err != nil {
				return err
			}
			continue
		case isKeyword(t, "on") || isKeyword(t, "using"):
			if _, err := s.scanUntil(joinConditionStop); err != nil {
				return err
			}
			continue
		case isClauseEnd(t) || t.typ == "eof":
			return nil
		default:
			if err := s.parseTableFactor(); err != nil {
				return err
			}
		}
	}
}

func (s *queryScanner) consumeJoinOp() error {
	for {
		t, err := s.peek()
		if err != nil {
			return err
		}
		if isKeyword(t, "join") {
			s.next()
			return nil
		}
		if isJoinOp(t) {
			s.next()
			continue
		}
		return invalidf("expected JOIN, got %q", t.val)
	}
}

func (s *queryScanner) parseTableFactor() error {
	t, err := s.peek()
	if err != nil {
		return err
	}

	if t.val == "(" {
		s.next() // consume (

		t2, _ := s.peek()
		switch {
		case isKeyword(t2, "select") || isKeyword(t2, "with") || isKeyword(t2, "values"):
			if err := s.parseStatement(); err != nil {
				return err
			}
			if err := s.expect(")"); err != nil {
				return err
			}
			return s.skipOptionalAlias()
		default:
			// Parenthesized table or join list: (notes), (a, b), (a JOIN b).
			if err := s.parseTableList(); err != nil {
				return err
			}
			if err := s.expect(")"); err != nil {
				return err
			}
			return s.skipOptionalAlias()
		}
	}

	if t.typ != "ident" {
		return invalidf("expected table name, got %q", t.val)
	}
	s.next()

	schema := ""
	name := t.val
	if t2, _ := s.peek(); t2.val == "." {
		s.next() // dot
		t3, err := s.next()
		if err != nil {
			return err
		}
		if t3.typ != "ident" {
			return invalidf("expected table name after '.', got %q", t3.val)
		}
		schema = name
		name = t3.val
	}

	// Table-valued functions (e.g. json_each(...)) use an identifier followed
	// by an argument list. The function name itself is not a table reference,
	// but we still need to skip the argument list.
	if t2, _ := s.peek(); t2.val == "(" {
		if isPragmaFunction(name) {
			if err := s.parsePragmaArgs(schema, name); err != nil {
				return err
			}
			return s.skipOptionalAlias()
		}
		if err := s.scanParenthesized(); err != nil {
			return err
		}
	} else if isPragmaFunction(name) {
		// Pragma virtual tables with no argument list (e.g. pragma_table_list)
		// can enumerate internal tables; reject them outright.
		return invalidf("query references reserved pragma %q", unquoteIdent(name))
	}

	if err := s.checkTableName(schema, name); err != nil {
		return err
	}

	return s.skipOptionalAlias()
}

func (s *queryScanner) checkTableName(schema, rawName string) error {
	name := unquoteIdent(rawName)
	if schema != "" {
		// Always check the unqualified table name; the schema prefix cannot
		// turn a reserved table into a normal one.
		name = unquoteIdent(schema) + "." + name
	}
	base := name
	if i := strings.LastIndex(base, "."); i >= 0 {
		base = base[i+1:]
	}
	if !isUserTable(base) {
		return invalidf("query references reserved table %q", base)
	}
	return nil
}

func isUserTable(name string) bool {
	return schema.ValidTableName(strings.ToLower(name))
}

func (s *queryScanner) skipOptionalAlias() error {
	t, err := s.peek()
	if err != nil {
		return err
	}
	if isKeyword(t, "as") {
		s.next() // consume AS
		t, err = s.next()
		if err != nil {
			return err
		}
		if t.typ != "ident" {
			return invalidf("expected alias after AS, got %q", t.val)
		}
		return nil
	}

	if t.typ != "ident" || isQuotedIdent(t.val) {
		return nil
	}
	kw := strings.ToLower(t.val)
	if isJoinOp(t) || isClauseEnd(t) || kw == "on" || kw == "using" || t.val == ")" || t.val == "," {
		return nil
	}
	s.next()
	return nil
}
