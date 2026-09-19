package postgres

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lsm/dolmen/internal/store"
)

type sqlNames struct {
	tables  []tableState
	encoded map[string]string
	decoded map[string]string
	used    map[string]bool
}

func newSQLNames(tables map[string]tableState) *sqlNames {
	n := &sqlNames{encoded: map[string]string{}, decoded: map[string]string{}, used: map[string]bool{}}
	for name, t := range tables {
		n.used[name] = true
		for _, f := range t.schema.Fields {
			n.used[f.Name] = true
		}
	}
	return n
}

func (n *sqlNames) name(logical string) string {
	if len(logical) <= 63 {
		return logical
	}
	if physical := n.encoded[logical]; physical != "" {
		return physical
	}
	for i := len(n.encoded); ; i++ {
		candidate := "dolmen_long_" + strconv.Itoa(i)
		if !n.used[candidate] {
			n.used[candidate] = true
			n.encoded[logical] = candidate
			n.decoded[candidate] = logical
			return candidate
		}
	}
}

func (n *sqlNames) original(name string) string {
	if logical := n.decoded[name]; logical != "" {
		return logical
	}
	return name
}

func sqlIdentifierStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func sqlIdentifierPart(r rune) bool  { return sqlIdentifierStart(r) || unicode.IsDigit(r) || r == '$' }

func rewriteSQL(input string, names *sqlNames) (string, int, error) {
	type part struct {
		raw        string
		identifier string
	}
	parts := []part{}
	parameters := 0
	fail := func() (string, int, error) {
		return "", 0, fmt.Errorf("%w: unterminated SQL literal, identifier, or comment", store.ErrInvalid)
	}
	for i := 0; i < len(input); {
		start := i
		switch {
		case strings.HasPrefix(input[i:], "--"):
			i += 2
			for i < len(input) && input[i] != '\n' {
				i++
			}
			parts = append(parts, part{raw: input[start:i]})
		case strings.HasPrefix(input[i:], "/*"):
			i += 2
			depth := 1
			for i < len(input) && depth > 0 {
				if strings.HasPrefix(input[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(input[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return fail()
			}
			parts = append(parts, part{raw: input[start:i]})
		case input[i] == '\'':
			escaped := i > 0 && (input[i-1] == 'e' || input[i-1] == 'E') && (i == 1 || !sqlIdentifierPart(rune(input[i-2])))
			i++
			closed := false
			for i < len(input) {
				if escaped && input[i] == '\\' {
					i += 2
					continue
				}
				if input[i] == '\'' {
					i++
					if i < len(input) && input[i] == '\'' {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed || i > len(input) {
				return fail()
			}
			parts = append(parts, part{raw: input[start:i]})
		case input[i] == '"':
			i++
			var name strings.Builder
			closed := false
			for i < len(input) {
				if input[i] == '"' {
					i++
					if i < len(input) && input[i] == '"' {
						name.WriteByte('"')
						i++
						continue
					}
					closed = true
					break
				}
				name.WriteByte(input[i])
				i++
			}
			if !closed {
				return fail()
			}
			names.used[name.String()] = true
			parts = append(parts, part{identifier: name.String(), raw: input[start:i]})
		case input[i] == '$':
			end := i + 1
			for end < len(input) && ((input[end] >= 'a' && input[end] <= 'z') || (input[end] >= 'A' && input[end] <= 'Z') || input[end] == '_' || (end > i+1 && input[end] >= '0' && input[end] <= '9')) {
				end++
			}
			if end < len(input) && input[end] == '$' {
				tag := input[i : end+1]
				at := strings.Index(input[end+1:], tag)
				if at < 0 {
					return fail()
				}
				i = end + 1 + at + len(tag)
				parts = append(parts, part{raw: input[start:i]})
			} else {
				return "", 0, fmt.Errorf("%w: use ? placeholders instead of numbered PostgreSQL parameters", store.ErrInvalid)
			}
		case input[i] == '?':
			if i+1 < len(input) && (input[i+1] == '?' || input[i+1] == '|' || input[i+1] == '&') {
				op := input[i : i+2]
				if op == "??" {
					op = "?"
				}
				parts = append(parts, part{raw: op})
				i += 2
			} else {
				parameters++
				parts = append(parts, part{raw: "$" + strconv.Itoa(parameters)})
				i++
			}
		default:
			r, size := utf8.DecodeRuneInString(input[i:])
			if sqlIdentifierStart(r) {
				i += size
				for i < len(input) {
					next, n := utf8.DecodeRuneInString(input[i:])
					if !sqlIdentifierPart(next) {
						break
					}
					i += n
				}
				name := strings.ToLower(input[start:i])
				names.used[name] = true
				parts = append(parts, part{raw: input[start:i], identifier: name})
			} else {
				parts = append(parts, part{raw: input[i : i+size]})
				i += size
			}
		}
	}
	var out strings.Builder
	for _, p := range parts {
		if p.identifier != "" && len(p.identifier) > 63 {
			out.WriteString(ident(names.name(p.identifier)))
		} else {
			out.WriteString(p.raw)
		}
	}
	return out.String(), parameters, nil
}
