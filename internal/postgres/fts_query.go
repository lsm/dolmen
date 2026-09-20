package postgres

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ftsConfig = "english"

type tsNode interface{}

type tsTerm struct {
	text   string
	prefix bool
}

type tsPhrase struct{ text string }

type tsBinary struct {
	op          byte
	left, right tsNode
}

type ftsToken struct {
	kind byte
	text string
	star bool
}

func ftsTokenize(match string) ([]ftsToken, error) {
	out := []ftsToken{}
	i := 0
	for i < len(match) {
		r, size := utf8.DecodeRuneInString(match[i:])
		switch {
		case unicode.IsSpace(r):
			i += size
		case r == '(':
			out = append(out, ftsToken{kind: '('})
			i += size
		case r == ')':
			out = append(out, ftsToken{kind: ')'})
			i += size
		case r == '"':
			i += size
			start := i
			for i < len(match) && match[i] != '"' {
				i++
			}
			if i >= len(match) {
				return nil, invalidf("query %q: unterminated double-quoted phrase", match)
			}
			out = append(out, ftsToken{kind: 'p', text: match[start:i]})
			i++
		case r == ':':
			return nil, invalidf("query %q: PostgreSQL full-text search does not support the %s column filter; drop it and filter with the filter parameter instead", match, "field:term")
		case r == '{':
			return nil, invalidf("query %q: PostgreSQL full-text search does not support the %s column-group filter; drop it and filter with the filter parameter instead", match, "{field field}:term")
		case r == '\'':
			return nil, invalidf("query %q: bare single quotes are not a term; double-quote terms that contain punctuation", match)
		default:
			start := i
			star := false
			for i < len(match) {
				next, n := utf8.DecodeRuneInString(match[i:])
				if unicode.IsSpace(next) || next == '(' || next == ')' || next == '"' || next == ':' || next == '{' || next == '\'' {
					break
				}
				if next == '*' {
					star = true
					i += n
					break
				}
				i += n
			}
			word := match[start:i]
			if star {
				word = strings.TrimSuffix(word, "*")
			}
			if word == "" {
				return nil, invalidf("query %q: a bare %q is not a term", match, "*")
			}
			switch word {
			case "AND":
				out = append(out, ftsToken{kind: '&'})
			case "OR":
				out = append(out, ftsToken{kind: '|'})
			case "NOT":
				out = append(out, ftsToken{kind: '!'})
			case "NEAR":
				return nil, invalidf("query %q: PostgreSQL full-text search has no proximity operator equivalent to %s; use a double-quoted phrase for adjacent words", match, "NEAR()")
			default:
				out = append(out, ftsToken{kind: 'w', text: word, star: star})
			}
		}
	}
	return out, nil
}

type ftsParser struct {
	tokens []ftsToken
	pos    int
	match  string
}

func (p *ftsParser) peek() (ftsToken, bool) {
	if p.pos >= len(p.tokens) {
		return ftsToken{}, false
	}
	return p.tokens[p.pos], true
}

func (p *ftsParser) primary() (tsNode, error) {
	tok, ok := p.peek()
	if !ok {
		return nil, invalidf("query %q: expected a search term", p.match)
	}
	switch tok.kind {
	case 'w':
		p.pos++
		return tsTerm{text: tok.text, prefix: tok.star}, nil
	case 'p':
		p.pos++
		return tsPhrase{text: tok.text}, nil
	case '(':
		p.pos++
		inner, err := p.orExpr()
		if err != nil {
			return nil, err
		}
		next, ok := p.peek()
		if !ok || next.kind != ')' {
			return nil, invalidf("query %q: unbalanced parentheses", p.match)
		}
		p.pos++
		return inner, nil
	}
	return nil, invalidf("query %q: expected a search term", p.match)
}

func (p *ftsParser) notExpr() (tsNode, error) {
	left, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		tok, ok := p.peek()
		if !ok || tok.kind != '!' {
			return left, nil
		}
		p.pos++
		right, err := p.primary()
		if err != nil {
			return nil, err
		}
		left = tsBinary{op: '!', left: left, right: right}
	}
}

func (p *ftsParser) andExpr() (tsNode, error) {
	left, err := p.notExpr()
	if err != nil {
		return nil, err
	}
	for {
		tok, ok := p.peek()
		if !ok || tok.kind == ')' || tok.kind == '|' {
			return left, nil
		}
		if tok.kind == '&' {
			p.pos++
		}
		right, err := p.notExpr()
		if err != nil {
			return nil, err
		}
		left = tsBinary{op: '&', left: left, right: right}
	}
}

func (p *ftsParser) orExpr() (tsNode, error) {
	left, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	for {
		tok, ok := p.peek()
		if !ok || tok.kind != '|' {
			return left, nil
		}
		p.pos++
		right, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		left = tsBinary{op: '|', left: left, right: right}
	}
}

func parseFTSQuery(match string) (tsNode, error) {
	tokens, err := ftsTokenize(match)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, invalidf("query %q: no search terms", match)
	}
	p := &ftsParser{tokens: tokens, match: match}
	node, err := p.orExpr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.tokens) {
		return nil, invalidf("query %q: unbalanced parentheses", match)
	}
	return node, nil
}

func simpleLexeme(term string) bool {
	for _, r := range term {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return term != ""
}

type ftsCompiler struct {
	args  []any
	match string
	err   error
}

func (c *ftsCompiler) param(v any) string {
	c.args = append(c.args, v)
	return "$" + strconv.Itoa(len(c.args))
}

func (c *ftsCompiler) emit(node tsNode) string {
	switch n := node.(type) {
	case tsTerm:
		if !n.prefix {
			return "plainto_tsquery('" + ftsConfig + "'," + c.param(n.text) + ")"
		}
		if !simpleLexeme(n.text) {
			if c.err == nil {
				c.err = invalidf("query %q: prefix search needs a plain word before %q; double-quote %q to search it as a phrase instead", c.match, "*", n.text)
			}
			return "plainto_tsquery('" + ftsConfig + "'," + c.param(n.text) + ")"
		}
		return "to_tsquery('" + ftsConfig + "'," + c.param("'"+n.text+"':*") + ")"
	case tsPhrase:
		return "phraseto_tsquery('" + ftsConfig + "'," + c.param(n.text) + ")"
	case tsBinary:
		left := c.emit(n.left)
		right := c.emit(n.right)
		switch n.op {
		case '|':
			return "(" + left + " || " + right + ")"
		case '!':
			return "(" + left + " && !! " + right + ")"
		}
		return "(" + left + " && " + right + ")"
	}
	return ""
}

func compileFTSQuery(match string, offset int) (string, []any, error) {
	node, err := parseFTSQuery(match)
	if err != nil {
		return "", nil, err
	}
	c := &ftsCompiler{args: make([]any, offset), match: match}
	sql := c.emit(node)
	if c.err != nil {
		return "", nil, c.err
	}
	return sql, c.args[offset:], nil
}
