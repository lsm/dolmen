package filter

import (
	"fmt"
	"sort"
	"strings"
)

type Error struct {
	Pos     int
	Message string
}

func (e *Error) Error() string { return e.Message }

func errAt(pos int, format string, args ...any) error {
	return &Error{Pos: pos, Message: fmt.Sprintf(format, args...)}
}

var allowedFunctions = map[string]argRange{
	"abs":       {1, 1},
	"round":     {1, 2},
	"length":    {1, 1},
	"lower":     {1, 1},
	"upper":     {1, 1},
	"substr":    {2, 3},
	"trim":      {1, 2},
	"ltrim":     {1, 2},
	"rtrim":     {1, 2},
	"replace":   {3, 3},
	"instr":     {2, 2},
	"coalesce":  {2, -1},
	"ifnull":    {2, 2},
	"nullif":    {2, 2},
	"iif":       {3, 3},
	"date":      {1, -1},
	"time":      {1, -1},
	"datetime":  {1, -1},
	"julianday": {1, -1},
	"strftime":  {2, -1},
}

var timeFunctions = map[string]bool{
	"date": true, "time": true, "datetime": true, "julianday": true, "strftime": true,
}

var rejectedTimeWords = map[string]string{
	"now":       "reads the server's clock",
	"localtime": "reads the server's time zone",
	"utc":       "reads the server's time zone",
}

var rejectedKeywords = map[string]string{
	"select": "a filter expression may not read rows itself; it sees only the row it is applied to",
	"from":   "a filter expression may not name a table; it sees only the row it is applied to",
	"join":   "a filter expression may not name a table; it sees only the row it is applied to",
	"exists": "a filter expression may not read rows itself; it sees only the row it is applied to",
	"union":  "a filter expression may not read rows itself; it sees only the row it is applied to",
	"with":   "a filter expression may not read rows itself; it sees only the row it is applied to",
	"values": "a filter expression may not read rows itself; it sees only the row it is applied to",
	"cast":   "cast is not one of the functions a filter expression may use",
	"raise":  "raise is not one of the functions a filter expression may use",
	"collate": "collate is not accepted in a filter expression; text comparison always uses byte order, " +
		"so the same filter means the same thing on every backend",
	"glob":              "glob is not an operator a filter expression may use; use like",
	"regexp":            "regexp is not an operator a filter expression may use; use like",
	"match":             "match is not an operator a filter expression may use; use like",
	"isnull":            "isnull is not an operator a filter expression may use; write is null",
	"notnull":           "notnull is not an operator a filter expression may use; write is not null",
	"current_timestamp": "current_timestamp reads the server's clock; compute the moment you mean and bind it as a ? argument",
	"current_date":      "current_date reads the server's clock; compute the moment you mean and bind it as a ? argument",
	"current_time":      "current_time reads the server's clock; compute the moment you mean and bind it as a ? argument",
}

const (
	MaxNestingDepth = 1000
	MaxFilterLength = 64 << 10
)

type argRange struct{ min, max int }

type Options struct {
	Columns []string
	Args    []any
}

func Validate(expr string, opts Options) error {
	_, err := Parse(expr, opts)
	return err
}

func Parse(expr string, opts Options) (Node, error) {
	if len(expr) > MaxFilterLength {
		return nil, errAt(0, "the filter expression is %d bytes, over the %d-byte limit", len(expr), MaxFilterLength)
	}
	columns := make(map[string]bool, len(opts.Columns))
	for _, c := range opts.Columns {
		columns[strings.ToLower(c)] = true
	}
	p := &parser{
		lex:     lexer{src: expr},
		columns: columns,
		args:    opts.Args,
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind == tokenEOF {
		return nil, errAt(0, "the filter expression is empty")
	}
	if err := p.expression(precedenceLowest); err != nil {
		return nil, err
	}
	if p.tok.kind != tokenEOF {
		if p.tok.is(";") {
			return nil, errAt(p.tok.pos, "a filter is one expression; it may not carry a second statement")
		}
		return nil, errAt(p.tok.pos, "%s is left over after the end of the filter expression", p.tok.describe())
	}
	if p.params != len(opts.Args) {
		return nil, errAt(0, "the filter expression has %d ? placeholder(s) but %d argument(s) were supplied", p.params, len(opts.Args))
	}
	if len(p.stack) != 1 {
		return nil, errAt(0, "the filter expression did not parse to a single value")
	}
	return p.stack[0], nil
}

func (p *parser) unknownColumn(name string, pos int) error {
	if len(p.columns) == 0 {
		return errAt(pos, "%q is not a column of this table", name)
	}
	known := make([]string, 0, len(p.columns))
	for c := range p.columns {
		known = append(known, c)
	}
	sort.Strings(known)
	if len(known) > 12 {
		known = append(known[:12:12], "…")
	}
	return errAt(pos, "%q is not a column of this table, and a filter expression may only name its own table's columns (%s)",
		name, strings.Join(known, ", "))
}
