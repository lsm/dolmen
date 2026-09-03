package store

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lsm/dolmen/internal/schema"
)

// validateQueryTables checks that a raw SELECT/WITH statement does not
// reference internal or reserved tables. It walks the statement without
// executing it, so it cannot be bypassed by quoting or string literals.
func validateQueryTables(stmt string) error {
	if utf8.RuneCountInString(stmt) > MaxQueryRunes {
		return invalidf("query exceeds maximum length")
	}
	s := newQueryScanner(stmt)
	if err := s.parseStatement(); err != nil {
		return err
	}
	return nil
}

// queryScanner is a small SQL tokenizer/parser used only to locate table
// references in SELECT/WITH statements.
const (
	maxTableParens = 50
	maxStmtDepth   = 20
	// MaxQueryRunes is the maximum number of Unicode characters in a SQL
	// statement accepted by the query validator. This matches JSON Schema's
	// maxLength semantics.
	MaxQueryRunes = 1 << 20 // 1 M characters
)

type queryScanner struct {
	s       string
	i       int
	buf     *token
	// cteScope is a stack of CTE name scopes. The top scope is the current
	// statement; lookups fall through to enclosing scopes so CTEs defined in an
	// outer WITH are visible inside their bodies, while inner CTEs do not leak.
	cteScope []map[string]bool
	// tableParens tracks the current depth of parenthesized table factors so
	// that a query with millions of nested parentheses cannot overflow the Go
	// stack or pin CPU before SQLite rejects it.
	tableParens int
	// stmtDepth tracks the current nesting depth of SELECT/WITH/VALUES
	// subqueries and CTE bodies.
	stmtDepth int
}

type token struct {
	typ string
	val string
}

func newQueryScanner(stmt string) *queryScanner {
	return &queryScanner{s: stmt}
}

func (s *queryScanner) pushCteScope() {
	s.cteScope = append(s.cteScope, make(map[string]bool))
}

func (s *queryScanner) popCteScope() {
	if n := len(s.cteScope); n > 0 {
		s.cteScope = s.cteScope[:n-1]
	}
}

func (s *queryScanner) addCteName(name string) {
	if n := len(s.cteScope); n == 0 {
		s.pushCteScope()
	}
	s.cteScope[len(s.cteScope)-1][name] = true
}

func (s *queryScanner) isCteName(name string) bool {
	for i := len(s.cteScope) - 1; i >= 0; i-- {
		if s.cteScope[i][name] {
			return true
		}
	}
	return false
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
	// ASCII case-insensitive like isKeyword; ToLower is the identity on the
	// punctuation values expect is called with.
	if strings.ToLower(t.val) == kw {
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
		case ':', '@', '$':
			// SQLite scans :name/@name/$name as a single variable token,
			// including Tcl-style :: suffixes ($x::from is one name), so a
			// keyword can never appear inside one. Tokenize them atomically or
			// SELECT schema_json, $x::from FROM _dolmen_tables would smuggle a
			// fake FROM.
			if s.i+1 < len(s.s) && (isIdentCont(s.s[s.i+1]) || s.atTclSuffix(s.i+1)) {
				return token{typ: "param", val: s.readParam()}, nil
			}
			s.i++
			return token{typ: "op", val: string(c)}, nil
		case '?':
			// ?NNN binds atomically; SQLite scans only digits after '?'.
			if s.i+1 < len(s.s) && unicode.IsDigit(rune(s.s[s.i+1])) {
				return token{typ: "param", val: s.readNumberedParam()}, nil
			}
			s.i++
			return token{typ: "op", val: string(c)}, nil
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
	// SQLite's whitespace set: space, tab, linefeed, carriage return, and
	// form feed (vertical tab is not whitespace and errors in SQLite).
	for s.i < len(s.s) {
		c := s.s[s.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' {
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

// isIdentStart reports whether c can begin an unquoted identifier. Like
// SQLite's tokenizer, every byte above ASCII counts as an identifier
// character, so non-ASCII CTE and alias names (e.g. 日本語) tokenize as
// identifiers instead of a run of operator bytes.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= utf8.RuneSelf
}

// isIdentCont reports whether c can continue an unquoted identifier. Like
// SQLite's IdChar, '$' is a continuation character (x$from is one name), so
// keywords cannot be recognized inside dollar-containing identifiers.
func isIdentCont(c byte) bool {
	return isIdentStart(c) || c == '$' || unicode.IsDigit(rune(c))
}

func (s *queryScanner) readIdent() string {
	start := s.i
	for s.i < len(s.s) && isIdentCont(s.s[s.i]) {
		s.i++
	}
	return s.s[start:s.i]
}

// readParam consumes a named variable token (:name, @name, $name) as one
// unit, mirroring SQLite so keywords inside parameter names stay inert. The
// name may carry Tcl-style :: suffixes: $x::ns::y is a single parameter.
func (s *queryScanner) readParam() string {
	start := s.i
	s.i++ // prefix character
	for s.i < len(s.s) && isIdentCont(s.s[s.i]) {
		s.i++
	}
	for s.atTclSuffix(s.i) {
		s.i += 2 // '::'
		for s.i < len(s.s) && isIdentCont(s.s[s.i]) {
			s.i++
		}
	}
	return s.s[start:s.i]
}

// atTclSuffix reports whether a '::' namespace separator starts at index i.
func (s *queryScanner) atTclSuffix(i int) bool {
	return i+1 < len(s.s) && s.s[i] == ':' && s.s[i+1] == ':'
}

// readNumberedParam consumes ?NNN, digits only, matching SQLite.
func (s *queryScanner) readNumberedParam() string {
	start := s.i
	s.i++ // '?'
	for s.i < len(s.s) && unicode.IsDigit(rune(s.s[s.i])) {
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

// isKeyword reports whether token t is the given lowercase ASCII keyword.
// SQLite compares keywords ASCII-case-insensitively, so compare lowercased
// values rather than strings.EqualFold, whose Unicode fold orbits would make
// "ſrom" equal "from".
func isKeyword(t token, kw string) bool {
	return t.typ == "ident" && !isQuotedIdent(t.val) && strings.ToLower(t.val) == kw
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
	// Table-valued PRAGMAs accept an optional second literal that selects the
	// schema (e.g. pragma_table_info('notes', 'main')). It does not change the
	// table name being inspected.
	if t, _ := s.peek(); t.val == "," {
		s.next()
		schemaArg, err := s.next()
		if err != nil {
			return err
		}
		if schemaArg.typ != "string" {
			return invalidf("pragma schema argument must be a string literal")
		}
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
	"join":      true,
}

var clauseEndStop = map[string]bool{
	"where":     true,
	"group":     true,
	"having":    true,
	"order":     true,
	"limit":     true,
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

// isWindowClause peeks ahead without consuming the current token. It reports
// true when the current identifier is the keyword WINDOW introducing a named
// window definition (WINDOW <name> AS ...), as opposed to WINDOW used as a
// table alias.
func (s *queryScanner) isWindowClause() bool {
	start := s.i
	startBuf := s.buf
	// Consume WINDOW.
	t, err := s.next()
	if err != nil || !isKeyword(t, "window") {
		s.i, s.buf = start, startBuf
		return false
	}
	name, err := s.next()
	if err != nil || (name.typ != "ident" && name.typ != "string") {
		s.i, s.buf = start, startBuf
		return false
	}
	as, err := s.next()
	if err != nil {
		s.i, s.buf = start, startBuf
		return false
	}
	s.i, s.buf = start, startBuf
	return isKeyword(as, "as")
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
	if s.stmtDepth >= maxStmtDepth {
		return invalidf("query statement nesting too deep")
	}
	s.stmtDepth++
	defer func() { s.stmtDepth-- }()

	// Push a fresh CTE scope so names introduced here do not leak outside this
	// statement (e.g. out of a scalar subquery).
	s.pushCteScope()
	defer s.popCteScope()

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
	// Pre-scan the CTE list to register every name before any body is parsed.
	// SQLite allows forward references, so a later CTE can be referenced by an
	// earlier one.
	fwd := *s
	fwd.buf = nil
	names, err := fwd.collectCteNames()
	if err != nil {
		return err
	}
	for k := range names {
		s.addCteName(k)
	}

	if t, _ := s.peek(); isKeyword(t, "recursive") {
		s.next()
	}
	for {
		t, err := s.next()
		if err != nil {
			return err
		}
		if t.typ != "ident" && t.typ != "string" {
			return invalidf("expected CTE name, got %q", t.val)
		}
		name := unquoteIdent(t.val)
		if t.typ == "string" {
			name = unquoteString(t.val)
		}
		s.addCteName(strings.ToLower(name))
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

// collectCteNames scans a WITH clause without validating CTE bodies and returns
// the set of CTE names. It is used by parseWith to register all names before
// any body is parsed so that forward references between CTEs resolve correctly.
func (s *queryScanner) collectCteNames() (map[string]bool, error) {
	names := make(map[string]bool)
	if t, _ := s.peek(); isKeyword(t, "recursive") {
		s.next()
	}
	for {
		t, err := s.next()
		if err != nil {
			return nil, err
		}
		if t.typ != "ident" && t.typ != "string" {
			return nil, invalidf("expected CTE name, got %q", t.val)
		}
		name := unquoteIdent(t.val)
		if t.typ == "string" {
			name = unquoteString(t.val)
		}
		names[strings.ToLower(name)] = true
		if t2, _ := s.peek(); t2.val == "(" {
			if err := s.skipParenthesized(); err != nil {
				return nil, err
			}
		}
		if err := s.expect("as"); err != nil {
			return nil, err
		}
		if t, _ := s.peek(); isKeyword(t, "not") {
			s.next()
			if err := s.expect("materialized"); err != nil {
				return nil, err
			}
		} else if isKeyword(t, "materialized") {
			s.next()
		}
		if err := s.skipParenthesized(); err != nil {
			return nil, err
		}
		if t2, _ := s.peek(); t2.val == "," {
			s.next()
			continue
		}
		return names, nil
	}
}

// skipParenthesized consumes a matching ) without interpreting the contents.
// It is used during the CTE pre-scan so nested subqueries are not parsed twice.
func (s *queryScanner) skipParenthesized() error {
	if err := s.expect("("); err != nil {
		return err
	}
	depth := 1
	for {
		t, err := s.next()
		if err != nil {
			return err
		}
		if t.typ == "eof" {
			return invalidf("unterminated parenthesized group")
		}
		if t.val == "(" {
			depth++
		} else if t.val == ")" {
			depth--
			if depth == 0 {
				return nil
			}
		}
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
		// expr IN table_name is shorthand for expr IN (SELECT * FROM table_name),
		// so a bare-table operand needs the same reserved-table check as a FROM
		// factor.
		if isKeyword(t, "in") {
			if err := s.checkInTableOperand(); err != nil {
				return token{}, err
			}
		}
	}
}

// checkInTableOperand validates the operand of IN when it is a bare table
// name (possibly schema-qualified or a table-valued function), mirroring the
// handling of table factors in parseTableFactor.
func (s *queryScanner) checkInTableOperand() error {
	t, err := s.peek()
	if err != nil {
		return err
	}
	if t.typ != "ident" && t.typ != "string" {
		// IN ( ... ) is handled by the surrounding scanners; anything else is
		// a syntax error SQLite will report.
		return nil
	}
	s.next()

	schema, name := "", t.val
	if t.typ == "string" {
		name = unquoteString(name)
	}
	if t2, _ := s.peek(); t2.val == "." {
		s.next() // dot
		t3, err := s.next()
		if err != nil {
			return err
		}
		if t3.typ != "ident" && t3.typ != "string" {
			return invalidf("expected table name after '.', got %q", t3.val)
		}
		schema, name = name, t3.val
		if t3.typ == "string" {
			name = unquoteString(name)
		}
	}
	// A CTE may shadow a pragma_* name, so check the CTE scope before treating
	// the identifier as a reserved pragma virtual table.
	isCTE := schema == "" && s.isCteName(strings.ToLower(unquoteIdent(name)))
	if t2, _ := s.peek(); t2.val == "(" {
		if !isCTE && isPragmaFunction(name) {
			if err := s.parsePragmaArgs(schema, name); err != nil {
				return err
			}
			return nil
		}
		return s.scanParenthesized()
	}
	if !isCTE && isPragmaFunction(name) {
		// A pragma virtual table with no argument list (e.g. pragma_table_list)
		// can enumerate internal tables; reject it outright.
		return invalidf("query references reserved pragma %q", unquoteIdent(name))
	}
	return s.checkTableName(schema, name)
}

func (s *queryScanner) scanParenthesized() error {
	if err := s.expect("("); err != nil {
		return err
	}
	t, err := s.peek()
	if err != nil {
		return err
	}
	if isKeyword(t, "select") || isKeyword(t, "with") || isKeyword(t, "values") {
		if err := s.parseStatement(); err != nil {
			return err
		}
		return s.expect(")")
	}

	// For expression groups, scan balanced parentheses iteratively so that
	// deeply nested ordinary parentheses cannot exhaust the Go stack or pin
	// CPU through recursive scanUntil/scanParenthesized calls. Statement
	// subqueries nested inside an expression group are still parsed when their
	// opening '(' is followed by SELECT/WITH/VALUES.
	depth := 1
	for {
		t, err := s.next()
		if err != nil {
			return err
		}
		if t.typ == "eof" {
			return invalidf("unterminated parenthesized group")
		}
		// Bare-table IN applies inside expression groups too.
		if isKeyword(t, "in") {
			if err := s.checkInTableOperand(); err != nil {
				return err
			}
		}
		if t.val == "(" {
			t2, err := s.peek()
			if err != nil {
				return err
			}
			if isKeyword(t2, "select") || isKeyword(t2, "with") || isKeyword(t2, "values") {
				// A statement subquery inside this expression group. parseStatement
				// consumes the statement and expect(")") closes the '(' we just
				// consumed; do not change depth because it is a balanced group of
				// its own.
				if err := s.parseStatement(); err != nil {
					return err
				}
				if err := s.expect(")"); err != nil {
					return err
				}
				continue
			}
			depth++
		} else if t.val == ")" {
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

func (s *queryScanner) parseTableList() error {
	// At the start of a table list (or after a comma or join operator) we expect
	// a table factor. Contextual join keywords such as LEFT, RIGHT, INNER, JOIN,
	// etc. can be valid table names in that position, so only treat them as join
	// operators when a table factor has already been parsed.
	sawTable := false
	for {
		t, err := s.peek()
		if err != nil {
			return err
		}

		switch {
		case t.val == ",":
			s.next()
			sawTable = false
			continue
		case isJoinOp(t):
			if !sawTable {
				if err := s.parseTableFactor(); err != nil {
					return err
				}
				sawTable = true
				continue
			}
			if err := s.consumeJoinOp(); err != nil {
				return err
			}
			sawTable = false
			continue
		case isKeyword(t, "on") || isKeyword(t, "using"):
			if !sawTable {
				if err := s.parseTableFactor(); err != nil {
					return err
				}
				sawTable = true
				continue
			}
			if _, err := s.scanUntil(joinConditionStop); err != nil {
				return err
			}
			continue
		case isKeyword(t, "window") && s.isWindowClause():
			return nil
		case isClauseEnd(t) || t.typ == "eof":
			return nil
		default:
			if err := s.parseTableFactor(); err != nil {
				return err
			}
			sawTable = true
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
		if s.tableParens >= maxTableParens {
			return invalidf("query table factor nesting too deep")
		}
		s.tableParens++
		defer func() { s.tableParens-- }()

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

	// SQLite allows legacy single-quoted identifiers in table-factor position.
	if t.typ != "ident" && t.typ != "string" {
		return invalidf("expected table name, got %q", t.val)
	}
	s.next()

	schema := ""
	name := t.val
	if t.typ == "string" {
		name = unquoteString(name)
	}
	if t2, _ := s.peek(); t2.val == "." {
		s.next() // dot
		t3, err := s.next()
		if err != nil {
			return err
		}
		if t3.typ != "ident" && t3.typ != "string" {
			return invalidf("expected table name after '.', got %q", t3.val)
		}
		schema = name
		name = t3.val
		if t3.typ == "string" {
			name = unquoteString(name)
		}
	}

	// Table-valued functions (e.g. json_each(...)) use an identifier followed
	// by an argument list. The function name itself is not a table reference,
	// but we still need to skip the argument list.
	// A CTE may shadow a pragma_* name, so check the CTE scope before treating
	// the identifier as a reserved pragma virtual table.
	isCTE := schema == "" && s.isCteName(strings.ToLower(unquoteIdent(name)))
	if t2, _ := s.peek(); t2.val == "(" {
		if !isCTE && isPragmaFunction(name) {
			if err := s.parsePragmaArgs(schema, name); err != nil {
				return err
			}
			return s.skipOptionalAlias()
		}
		if err := s.scanParenthesized(); err != nil {
			return err
		}
	} else if !isCTE && isPragmaFunction(name) {
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
	base = strings.ToLower(base)
	// A CTE name (possibly shadowing a reserved table) is not a physical table
	// reference, so allow it.
	if schema == "" && s.isCteName(base) {
		return nil
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
		if t.typ != "ident" && t.typ != "string" {
			return invalidf("expected alias after AS, got %q", t.val)
		}
		return nil
	}

	if t.typ != "ident" && t.typ != "string" {
		return nil
	}

	// Quoted identifiers and single-quoted strings can be aliases. Unquoted
	// identifiers need keyword disambiguation because they may introduce a join
	// operator or clause end.
	if t.typ == "ident" && !isQuotedIdent(t.val) {
		kw := strings.ToLower(t.val)
		if isJoinOp(t) || isClauseEnd(t) || kw == "on" || kw == "using" || t.val == ")" || t.val == "," {
			return nil
		}
		// WINDOW is a clause when it is followed by a name and AS; otherwise it can
		// be used as an implicit table alias (e.g. "FROM a window JOIN b").
		if isKeyword(t, "window") && s.isWindowClause() {
			return nil
		}
	}
	s.next()
	return nil
}
