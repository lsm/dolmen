package filter

import (
	"fmt"
	"strings"
)

type tokenKind int

const (
	tokenEOF tokenKind = iota
	tokenIdent
	tokenQuotedIdent
	tokenString
	tokenNumber
	tokenBlob
	tokenParam
	tokenOp
)

type token struct {
	kind tokenKind
	text string
	pos  int
}

func (t token) is(op string) bool {
	return t.kind == tokenOp && t.text == op
}

func (t token) keyword(word string) bool {
	return t.kind == tokenIdent && strings.EqualFold(t.text, word)
}

func (t token) describe() string {
	switch t.kind {
	case tokenEOF:
		return "the end of the expression"
	case tokenString:
		return "a text literal"
	case tokenNumber:
		return "a number"
	case tokenBlob:
		return "a blob literal"
	case tokenParam:
		return fmt.Sprintf("%q", t.text)
	default:
		return fmt.Sprintf("%q", t.text)
	}
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) skipSpace() error {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			l.pos++
		case c == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-':
			return errAt(l.pos, "comments are not accepted in a filter expression")
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*':
			return errAt(l.pos, "comments are not accepted in a filter expression")
		default:
			return nil
		}
	}
	return nil
}

func (l *lexer) next() (token, error) {
	if err := l.skipSpace(); err != nil {
		return token{}, err
	}
	if l.pos >= len(l.src) {
		return token{kind: tokenEOF, pos: l.pos}, nil
	}
	start := l.pos
	c := l.src[l.pos]

	switch {
	case c == '\'':
		text, err := l.quoted('\'')
		if err != nil {
			return token{}, err
		}
		return token{kind: tokenString, text: text, pos: start}, nil
	case c == '"':
		text, err := l.quoted('"')
		if err != nil {
			return token{}, err
		}
		return token{kind: tokenQuotedIdent, text: text, pos: start}, nil
	case c == '`':
		return token{}, errAt(start, "backtick-quoted names are not accepted in a filter expression; quote a column name with double quotes")
	case c == '[':
		return token{}, errAt(start, "bracket-quoted names are not accepted in a filter expression; quote a column name with double quotes")
	case c == '?':
		l.pos++
		if l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			return token{}, errAt(start, "numbered parameters are not accepted in a filter expression; use a bare ? for each argument, in order")
		}
		return token{kind: tokenParam, text: "?", pos: start}, nil
	case c == ':' || c == '@' || c == '$':
		return token{}, errAt(start, "named parameters are not accepted in a filter expression; use a bare ? for each argument, in order")
	case isDigit(c) || (c == '.' && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1])):
		return l.number()
	case (c == 'x' || c == 'X') && l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'':
		l.pos++
		text, err := l.quoted('\'')
		if err != nil {
			return token{}, err
		}
		return token{kind: tokenBlob, text: text, pos: start}, nil
	case isIdentStart(c):
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.pos++
		}
		return token{kind: tokenIdent, text: l.src[start:l.pos], pos: start}, nil
	}

	for _, op := range operators {
		if strings.HasPrefix(l.src[l.pos:], op) {
			l.pos += len(op)
			return token{kind: tokenOp, text: op, pos: start}, nil
		}
	}
	return token{}, errAt(start, "%q is not an operator a filter expression may use", string(c))
}

var operators = []string{
	"<<", ">>", "<=", ">=", "==", "!=", "<>", "||", "->>", "->",
	"(", ")", ",", "+", "-", "*", "/", "%", "<", ">", "=", "~", "&", "|", ".", ";",
}

func (l *lexer) quoted(delim byte) (string, error) {
	start := l.pos
	l.pos++
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == delim {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == delim {
				b.WriteByte(delim)
				l.pos += 2
				continue
			}
			l.pos++
			return b.String(), nil
		}
		b.WriteByte(c)
		l.pos++
	}
	return "", errAt(start, "the quoted text starting here is never closed")
}

func (l *lexer) number() (token, error) {
	start := l.pos
	if l.src[l.pos] == '0' && l.pos+1 < len(l.src) && (l.src[l.pos+1] == 'x' || l.src[l.pos+1] == 'X') {
		l.pos += 2
		for l.pos < len(l.src) && isHex(l.src[l.pos]) {
			l.pos++
		}
		if l.pos == start+2 {
			return token{}, errAt(start, "a hexadecimal literal must have at least one digit")
		}
		return token{kind: tokenNumber, text: l.src[start:l.pos], pos: start}, nil
	}
	seenDot, seenExp := false, false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case isDigit(c):
			l.pos++
		case c == '.' && !seenDot && !seenExp:
			seenDot = true
			l.pos++
		case (c == 'e' || c == 'E') && !seenExp && l.pos > start:
			seenExp = true
			l.pos++
			if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
				l.pos++
			}
			if l.pos >= len(l.src) || !isDigit(l.src[l.pos]) {
				return token{}, errAt(start, "the exponent of this number has no digits")
			}
		default:
			return token{kind: tokenNumber, text: l.src[start:l.pos], pos: start}, nil
		}
	}
	return token{kind: tokenNumber, text: l.src[start:l.pos], pos: start}, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }
