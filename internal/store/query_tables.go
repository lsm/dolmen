package store

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lsm/dolmen/internal/schema"
)

func validateQueryTables(stmt string, registered map[string]bool) error {
	_, err := scanQueryTables(stmt, registered)
	return err
}

type queryScan struct {
	rewrites []tableRewrite
}

func scanQueryTables(stmt string, registered map[string]bool) (queryScan, error) {
	if utf8.RuneCountInString(stmt) > MaxQueryRunes {
		return queryScan{}, invalidf("query exceeds maximum length")
	}
	s := newQueryScanner(stmt)
	s.registered = registered
	if err := s.parseStatement(); err != nil {
		return queryScan{}, err
	}
	return queryScan{rewrites: s.rewrites}, nil
}

func (q queryScan) refuseMaskedShapes(masked map[string]maskedTable) error {
	if len(masked) == 0 {
		return nil
	}
	for _, r := range q.rewrites {
		if _, ok := masked[r.table]; !ok {
			continue
		}
		if r.qualified {
			return invalidf("table %s holds secret fields, so query reads it through a masked subquery; refer to it as %s, without a schema prefix", r.table, r.table)
		}
		if r.hinted {
			return invalidf("table %s holds secret fields, so query reads it through a masked subquery that INDEXED BY and NOT INDEXED cannot apply to; drop the index hint", r.table)
		}
	}
	return nil
}

var maskedRowidErr = regexp.MustCompile(`no such column: (?:[A-Za-z_][A-Za-z0-9_]*\.)?(rowid|_rowid_|oid)\b`)

func maskedRowidRefusal(masked map[string]maskedTable, err error) error {
	if len(masked) == 0 || err == nil {
		return nil
	}
	m := maskedRowidErr.FindStringSubmatch(err.Error())
	if m == nil {
		return nil
	}
	return invalidf("this query reads a table holding secret fields through a masked subquery, which has no %s; use id, which holds the same value", m[1])
}

func maskSecretTables(stmt string, rewrites []tableRewrite, masked map[string]maskedTable) string {
	if len(masked) == 0 {
		return stmt
	}
	var sb strings.Builder
	last := 0
	for _, r := range rewrites {
		m, ok := masked[r.table]
		if !ok || r.start < last || r.end > len(stmt) {
			continue
		}
		sb.WriteString(stmt[last:r.start])
		sb.WriteString(m.projection)
		if !r.aliased {
			sb.WriteString(" AS " + q(r.table))
		}
		last = r.end
	}
	sb.WriteString(stmt[last:])
	return sb.String()
}

const (
	maxTableParens = 50
	maxStmtDepth   = 20

	MaxQueryRunes = 1 << 20
)

type queryScanner struct {
	s   string
	i   int
	buf *token

	registered map[string]bool

	tokenStart int

	rewrites []tableRewrite

	indexHint bool

	cteScope []map[string]bool

	tableParens int

	stmtDepth int
}

type token struct {
	typ   string
	val   string
	start int
	end   int
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

	if asciiLower(t.val) == kw {
		return nil
	}
	return invalidf("unexpected token %q, expected %q", t.val, kw)
}

func (s *queryScanner) scanToken() (token, error) {
	t, err := s.scanTokenAt()
	t.end = s.i
	return t, err
}

func (s *queryScanner) scanTokenAt() (token, error) {
	for {
		s.skipWhitespace()
		s.tokenStart = s.i
		if s.i >= len(s.s) {
			return token{typ: "eof", val: "", start: s.tokenStart}, nil
		}

		c := s.s[s.i]
		switch c {
		case '-':
			if s.i+1 < len(s.s) && s.s[s.i+1] == '-' {
				s.skipLineComment()
				continue
			}
			s.i++
			return token{typ: "op", val: "-", start: s.tokenStart}, nil
		case '/':
			if s.i+1 < len(s.s) && s.s[s.i+1] == '*' {
				s.skipBlockComment()
				continue
			}
			s.i++
			return token{typ: "op", val: "/", start: s.tokenStart}, nil
		case '\'':
			v, err := s.readSingleQuoted()
			if err != nil {
				return token{}, err
			}
			return token{typ: "string", val: v, start: s.tokenStart}, nil
		case '"':
			v, err := s.readQuoted(c)
			if err != nil {
				return token{}, err
			}
			return token{typ: "ident", val: v, start: s.tokenStart}, nil
		case '`':
			v, err := s.readQuoted(c)
			if err != nil {
				return token{}, err
			}
			return token{typ: "ident", val: v, start: s.tokenStart}, nil
		case '[':
			v, err := s.readBracketed()
			if err != nil {
				return token{}, err
			}
			return token{typ: "ident", val: v, start: s.tokenStart}, nil
		case '(':
			s.i++
			return token{typ: "punct", val: "(", start: s.tokenStart}, nil
		case ')':
			s.i++
			return token{typ: "punct", val: ")", start: s.tokenStart}, nil
		case ',':
			s.i++
			return token{typ: "punct", val: ",", start: s.tokenStart}, nil
		case ';':
			s.i++
			return token{typ: "punct", val: ";", start: s.tokenStart}, nil
		case '.':
			s.i++
			return token{typ: "punct", val: ".", start: s.tokenStart}, nil
		case ':', '@', '$', '#':

			if s.i+1 < len(s.s) && (isIdentCont(s.s[s.i+1]) || s.atTclSuffix(s.i+1)) {
				return token{typ: "param", val: s.readParam(), start: s.tokenStart}, nil
			}
			s.i++
			return token{typ: "op", val: string(c), start: s.tokenStart}, nil
		case '?':

			if s.i+1 < len(s.s) && unicode.IsDigit(rune(s.s[s.i+1])) {
				return token{typ: "param", val: s.readNumberedParam(), start: s.tokenStart}, nil
			}
			s.i++
			return token{typ: "op", val: string(c), start: s.tokenStart}, nil
		default:
			if isIdentStart(c) {
				return token{typ: "ident", val: s.readIdent(), start: s.tokenStart}, nil
			}
			if unicode.IsDigit(rune(c)) || (c == '.' && s.i+1 < len(s.s) && unicode.IsDigit(rune(s.s[s.i+1]))) {
				return token{typ: "number", val: s.readNumber(), start: s.tokenStart}, nil
			}
			s.i++
			return token{typ: "op", val: string(c), start: s.tokenStart}, nil
		}
	}
}

func (s *queryScanner) skipWhitespace() {

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
	s.i += 2
	for s.i < len(s.s) && s.s[s.i] != '\n' {
		s.i++
	}
}

func (s *queryScanner) skipBlockComment() {
	s.i += 2
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
	s.i++
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
	s.i++
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
	s.i++
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
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= utf8.RuneSelf
}

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

func (s *queryScanner) readParam() string {
	start := s.i
	s.i++
	for s.i < len(s.s) && isIdentCont(s.s[s.i]) {
		s.i++
	}
	for s.atTclSuffix(s.i) {
		s.i += 2
		for s.i < len(s.s) && isIdentCont(s.s[s.i]) {
			s.i++
		}
	}
	if s.i < len(s.s) && s.s[s.i] == '(' {
		s.i++
		for s.i < len(s.s) {
			c := s.s[s.i]
			if c == ')' {
				s.i++
				break
			}
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' {
				break
			}
			s.i++
		}
	}
	return s.s[start:s.i]
}

func (s *queryScanner) atTclSuffix(i int) bool {
	return i+1 < len(s.s) && s.s[i] == ':' && s.s[i+1] == ':'
}

func (s *queryScanner) readNumberedParam() string {
	start := s.i
	s.i++
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

func isKeyword(t token, kw string) bool {
	return t.typ == "ident" && !isQuotedIdent(t.val) && asciiLower(t.val) == kw
}

func unquoteString(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

func asciiLower(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func isPragmaFunction(rawName string) bool {
	return strings.HasPrefix(asciiLower(unquoteIdent(rawName)), "pragma_")
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
	return t.typ == "ident" && !isQuotedIdent(t.val) && stop[asciiLower(t.val)]
}

func isClauseEnd(t token) bool {
	if t.typ == "eof" {
		return true
	}
	if t.typ == "punct" {
		return t.val == ")" || t.val == ";"
	}
	return t.typ == "ident" && !isQuotedIdent(t.val) && clauseEndStop[asciiLower(t.val)]
}

func (s *queryScanner) isWindowClause() bool {
	start := s.i
	startBuf := s.buf

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
	switch asciiLower(t.val) {
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
		s.addCteName(asciiLower(name))
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
		names[asciiLower(name)] = true
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
			return invalidf("incomplete SQL statement: unterminated parenthesized group; only read-only SELECT or WITH statements are allowed")
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

	t, err := s.scanUntil(fromStop)
	if err != nil {
		return err
	}
	if isKeyword(t, "from") {
		s.next()
		if err := s.parseTableList(); err != nil {
			return err
		}
	}

	_, err = s.scanUntil(selectStop)
	return err
}

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

		if isKeyword(t, "in") {
			if err := s.checkInTableOperand(); err != nil {
				return token{}, err
			}
		}
	}
}

func (s *queryScanner) checkInTableOperand() error {
	t, err := s.peek()
	if err != nil {
		return err
	}
	if t.typ != "ident" && t.typ != "string" {

		return nil
	}
	s.next()

	schema, name := "", t.val
	if t.typ == "string" {
		name = unquoteString(name)
	}
	if t2, _ := s.peek(); t2.val == "." {
		s.next()
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

	isCTE := schema == "" && s.isCteName(asciiLower(unquoteIdent(name)))
	if t2, _ := s.peek(); t2.val == "(" {
		if !isCTE && isPragmaFunction(name) {
			return s.parsePragmaArgs(schema, name)
		}
		if err := s.scanParenthesized(); err != nil {
			return err
		}

		return s.checkTableName(schema, name)
	}
	if !isCTE && !s.isRegisteredRef(schema, name) && isPragmaFunction(name) {

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

	depth := 1
	for {
		t, err := s.next()
		if err != nil {
			return err
		}
		if t.typ == "eof" {
			return invalidf("incomplete SQL statement: unterminated parenthesized group; only read-only SELECT or WITH statements are allowed")
		}

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

func ownSchema(schema string) bool {
	if schema == "" {
		return true
	}
	return asciiLower(unquoteIdent(schema)) == "main"
}

func (s *queryScanner) isRegisteredRef(schema, name string) bool {
	return ownSchema(schema) && s.registered[asciiLower(unquoteIdent(name))]
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

		s.next()

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

			if err := s.parseTableList(); err != nil {
				return err
			}
			if err := s.expect(")"); err != nil {
				return err
			}
			return s.skipOptionalAlias()
		}
	}

	if t.typ != "ident" && t.typ != "string" {
		return invalidf("expected table name, got %q", t.val)
	}
	s.next()
	refEnd := t.end

	schema := ""
	name := t.val
	if t.typ == "string" {
		name = unquoteString(name)
	}
	if t2, _ := s.peek(); t2.val == "." {
		s.next()
		t3, err := s.next()
		if err != nil {
			return err
		}
		refEnd = t3.end
		if t3.typ != "ident" && t3.typ != "string" {
			return invalidf("expected table name after '.', got %q", t3.val)
		}
		schema = name
		name = t3.val
		if t3.typ == "string" {
			name = unquoteString(name)
		}
	}

	isCTE := schema == "" && s.isCteName(asciiLower(unquoteIdent(name)))
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
	} else if !isCTE && !s.isRegisteredRef(schema, name) && isPragmaFunction(name) {

		return invalidf("query references reserved pragma %q", unquoteIdent(name))
	}

	if err := s.checkTableName(schema, name); err != nil {
		return err
	}
	recorded := !isCTE && s.isRegisteredRef(schema, name)
	if recorded {
		s.rewrites = append(s.rewrites, tableRewrite{
			table:     asciiLower(unquoteIdent(name)),
			start:     t.start,
			end:       refEnd,
			qualified: schema != "",
		})
	}

	s.indexHint = false
	aliased, err := s.skipOptionalAliasReported()
	if err != nil {
		return err
	}
	if recorded {
		last := &s.rewrites[len(s.rewrites)-1]
		last.aliased = aliased
		last.hinted = s.indexHint
	}
	return nil
}

type tableRewrite struct {
	table     string
	start     int
	end       int
	aliased   bool
	qualified bool
	hinted    bool
}

func (s *queryScanner) checkTableName(schema, rawName string) error {
	name := unquoteIdent(rawName)
	if schema != "" {

		name = unquoteIdent(schema) + "." + name
	}
	base := asciiLower(name)

	if schema == "" && s.isCteName(base) {
		return nil
	}

	if i := strings.LastIndex(base, "."); i >= 0 {
		base = base[i+1:]
	}
	if ownSchema(schema) && s.registered[base] {
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
	_, err := s.skipOptionalAliasReported()
	return err
}

func (s *queryScanner) skipOptionalAliasReported() (bool, error) {
	t, err := s.peek()
	if err != nil {
		return false, err
	}

	if isKeyword(t, "indexed") || (isKeyword(t, "not") && s.isNotIndexed()) {
		return false, s.skipIndexedBy()
	}
	if isKeyword(t, "as") {
		s.next()
		t, err = s.next()
		if err != nil {
			return false, err
		}
		if t.typ != "ident" && t.typ != "string" {
			return false, invalidf("expected alias after AS, got %q", t.val)
		}
		return true, s.skipIndexedBy()
	}

	if t.typ != "ident" && t.typ != "string" {
		return false, nil
	}

	if t.typ == "ident" && !isQuotedIdent(t.val) {
		kw := asciiLower(t.val)
		if isJoinOp(t) || isClauseEnd(t) || kw == "on" || kw == "using" || t.val == ")" || t.val == "," {
			return false, nil
		}

		if isKeyword(t, "window") && s.isWindowClause() {
			return false, nil
		}
	}
	s.next()
	return true, s.skipIndexedBy()
}

func (s *queryScanner) skipIndexedBy() error {
	t, err := s.peek()
	if err != nil {
		return err
	}
	switch {
	case isKeyword(t, "indexed"):
		s.indexHint = true
		s.next()
		if err := s.expect("by"); err != nil {
			return err
		}
		idx, err := s.next()
		if err != nil {
			return err
		}
		if idx.typ != "ident" && idx.typ != "string" {
			return invalidf("expected index name after INDEXED BY, got %q", idx.val)
		}
	case isKeyword(t, "not") && s.isNotIndexed():
		s.indexHint = true
		s.next()
		if err := s.expect("indexed"); err != nil {
			return err
		}
	}
	return nil
}

func (s *queryScanner) isNotIndexed() bool {
	start, startBuf := s.i, s.buf
	defer func() { s.i, s.buf = start, startBuf }()
	if t, err := s.next(); err != nil || !isKeyword(t, "not") {
		return false
	}
	t2, err := s.next()
	return err == nil && isKeyword(t2, "indexed")
}
