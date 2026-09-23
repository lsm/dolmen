package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/lsm/dolmen/internal/value"
)

func normalizeArg(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return f
	}
	return v
}

var queryStartRe = regexp.MustCompile(`(?i)\A\s*(select|with)\b`)

func stripUnterminatedBlockComment(s string) string {
	var (
		inString, inLineComment, inBlockComment bool
		inIdent                                 bool
		identClose                              byte
		blockStart                              int
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
			continue
		}
		if inIdent {
			if c == identClose {
				if i+1 < len(s) && s[i+1] == identClose {
					i++
					continue
				}
				inIdent = false
			}
			continue
		}
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		switch c {
		case '\'':
			inString = true
		case '"':
			inIdent = true
			identClose = '"'
		case '`':
			inIdent = true
			identClose = '`'
		case '[':
			inIdent = true
			identClose = ']'
		case '-':
			if i+1 < len(s) && s[i+1] == '-' {
				inLineComment = true
				i++
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '*' {
				inBlockComment = true
				blockStart = i
				i++
			}
		}
	}
	if inBlockComment {
		return strings.TrimRight(s[:blockStart], " \t\r\n")
	}
	return s
}

func hasStatementSeparator(sql string) bool {
	var closing byte
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if closing != 0 {
			if c == closing {
				if c != ']' && i+1 < len(sql) && sql[i+1] == c {
					i++
					continue
				}
				closing = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			closing = c
		case '[':
			closing = ']'
		case ';':
			return true
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				for i < len(sql) && sql[i] != '\n' {
					i++
				}
			}
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' {
				i++
				for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
					i++
				}
				i++
			}
		}
	}
	return false
}

func firstKeyword(query string) string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return "an empty query"
	}
	end := strings.IndexFunc(trimmed, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '(' || r == ';'
	})
	if end < 0 {
		end = len(trimmed)
	}
	word := trimmed[:end]
	if len(word) > 24 {
		word = word[:24] + "..."
	}
	return strconv.Quote(word)
}

func ValidateQueryShape(query string) error {
	if !queryStartRe.MatchString(strings.TrimSpace(query)) {
		return invalidf("query must begin with SELECT or WITH (got %s); query is read-only, so writes go through insert/update/upsert/delete — and check the first keyword for a typo, which lands here too", firstKeyword(query))
	}
	trimmed := strings.TrimRight(strings.TrimSpace(query), ";")
	trimmed = stripUnterminatedBlockComment(trimmed)
	if hasStatementSeparator(trimmed) {
		return invalidf("multiple statements are not allowed")
	}
	return nil
}

func (s *Store) Query(ctx context.Context, nsName, query string, args []any, nsGen [16]byte, page Page) (QueryResult, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(query), ";")
	trimmed = stripUnterminatedBlockComment(trimmed)
	if err := ValidateQueryShape(query); err != nil {
		return QueryResult{}, err
	}
	if len(args) > 100 {
		return QueryResult{}, invalidf("too many query parameters")
	}
	limit := queryLimit(page.Limit)
	offset := page.Offset
	if offset < 0 {
		return QueryResult{}, invalidf("offset must be non-negative")
	}
	for i, a := range args {
		args[i] = normalizeArg(a)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return QueryResult{}, err
	}
	defer n.unpin()

	tx, done, err := beginCallerTx(ctx, n.ro, nil)
	if err != nil {
		return QueryResult{}, err
	}
	defer done()
	registered, err := registeredTables(ctx, tx)
	if err != nil {
		return QueryResult{}, err
	}
	if err := validateQueryTables(trimmed, registered); err != nil {
		return QueryResult{}, err
	}
	paginated := trimmed + "\nLIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)

	rows, err := tx.QueryContext(ctx, paginated, args...)
	if err != nil {

		first := err
		userArgs := args[:len(args)-2]
		if !strings.Contains(first.Error(), "no such table") {
			wrapped := "SELECT * FROM (\n" + trimmed + "\n)\nLIMIT ? OFFSET ?"
			rows, err = tx.QueryContext(ctx, wrapped, args...)
			if err == nil {

				if probe, perr := tx.QueryContext(ctx, trimmed, userArgs...); perr == nil {
					if cols, cerr := probe.Columns(); cerr == nil {
						seen := make(map[string]bool, len(cols))
						for _, c := range cols {
							if seen[c] {
								probe.Close()
								return QueryResult{}, invalidf("duplicate column label %q in query result; use AS aliases", c)
							}
							seen[c] = true
						}
					}
					probe.Close()
				}
				paginated = wrapped
			} else {
				if probe, bareErr := tx.QueryContext(ctx, trimmed, userArgs...); bareErr != nil {
					err = bareErr
				} else {
					probe.Close()
					err = first
				}
			}
		}
		if err != nil {
			return QueryResult{}, NewQueryError(trimmed, err)
		}
	}
	defer rows.Close()

	proj, err := s.nsProjection(ctx, tx, paginated)
	if err != nil {
		return QueryResult{}, err
	}
	rowsOut, truncated, err := rowsToMaps(rows, proj, limit)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Rows: rowsOut, Truncated: truncated}, nil
}

const (
	DefaultPageLimit = 1000
	MaxPageLimit     = 1000
)

const MaxQueryBytes = 32 << 20

func queryLimit(n int) int {
	if n <= 0 {
		return DefaultPageLimit
	}
	if n > MaxPageLimit {
		return MaxPageLimit
	}
	return n
}

func rowsToMaps(rows *sql.Rows, proj *projection, pageLimit int) ([]map[string]any, bool, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	labelBytes := 0
	for _, c := range cols {
		if seen[c] {
			return nil, false, invalidf("duplicate column label %q in query result; use AS aliases", c)
		}
		if len(c) > 4096 {
			return nil, false, invalidf("column label exceeds 4096 bytes; use a shorter AS alias")
		}
		seen[c] = true
		if proj.isHidden(c) {
			continue
		}
		labelBytes += encodedSize(c) + 16
	}
	out := []map[string]any{}
	total := 0
	hasMore := false
scan:
	for i := 0; i <= pageLimit; i++ {
		if !rows.Next() {
			break
		}
		if i == pageLimit {

			hasMore = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for j := range vals {
			ptrs[j] = &vals[j]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, false, err
		}
		m := make(map[string]any, len(cols))
		rowBytes := 0
		for j, c := range cols {
			if proj.isHidden(c) {
				continue
			}
			if err := checkRowValue(c, vals[j]); err != nil {
				return nil, false, err
			}
			if total+rowBytes+rawValSize(vals[j]) > MaxQueryBytes {
				if len(out) == 0 {
					return nil, false, invalidf("query result exceeds the %d MiB response budget on its first row; select fewer or smaller columns", MaxQueryBytes>>20)
				}
				hasMore = true
				break scan
			}
			v := proj.decodeColumn(c, vals[j])
			m[c] = v
			rowBytes += proj.presentedSize(c, vals[j], v)
			if total+rowBytes+labelBytes > MaxQueryBytes {
				if len(out) == 0 {
					return nil, false, invalidf("query result exceeds the %d MiB response budget on its first row; select fewer or smaller columns", MaxQueryBytes>>20)
				}
				hasMore = true
				break scan
			}
		}
		total += rowBytes + labelBytes
		out = append(out, m)
	}
	return out, hasMore, wrapStepErr(rows.Err())
}

func wrapStepErr(err error) error {
	if err == nil {
		return nil
	}
	return NewQueryError("", err)
}

func checkRowValue(col string, v any) error {
	switch t := v.(type) {
	case []byte:
		if len(t) > MaxQueryBytes {
			return invalidf("column %q exceeds the %d MiB response budget; select fewer or smaller columns", col, MaxQueryBytes>>20)
		}
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return invalidf("column %q produced a non-finite value", col)
		}
	}
	return nil
}

func rawValSize(v any) int     { return value.RawSize(v) }
func approxSize(v any) int     { return value.ApproxSize(v) }
func encodedSize(s string) int { return value.EncodedSize(s) }

func normalizeVal(v any) any {
	return value.Normalize(v)
}
