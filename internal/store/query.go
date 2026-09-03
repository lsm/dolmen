package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
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

// stripUnterminatedBlockComment removes a trailing /* block comment that is
// not closed before end-of-input. SQLite allows unterminated block comments to
// run to EOF, so appending pagination after one would place the LIMIT clause
// inside the comment. We strip the comment (which does not affect query
// semantics) and trim trailing whitespace so the pagination clause is appended
// to a complete statement. Strings and identifiers are skipped so a literal
// `/*` inside a quoted value is not treated as a comment.
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

// hasStatementSeparator reports whether sql contains a semicolon outside
// string literals, quoted identifiers, and comments — the only semicolons
// SQLite treats as statement terminators. Quoted or commented ones are
// content (WHERE title = 'a;b'), not a second statement.
func hasStatementSeparator(sql string) bool {
	var closing byte // 0 outside quotes; otherwise the mark closing the current quote
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if closing != 0 {
			if c == closing {
				if c != ']' && i+1 < len(sql) && sql[i+1] == c {
					i++ // doubled quote mark escapes itself
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
			if i+1 < len(sql) && sql[i+1] == '-' { // line comment
				for i < len(sql) && sql[i] != '\n' {
					i++
				}
			}
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' { // block comment
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

func (s *Store) Query(ctx context.Context, nsName, query string, args []any, offset, limit int) ([]map[string]any, bool, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(query), ";")
	trimmed = stripUnterminatedBlockComment(trimmed)
	if !queryStartRe.MatchString(strings.TrimSpace(query)) {
		return nil, false, invalidf("only read-only SELECT/WITH statements are allowed")
	}
	if hasStatementSeparator(trimmed) {
		return nil, false, invalidf("multiple statements are not allowed")
	}
	if len(args) > 100 {
		return nil, false, invalidf("too many query parameters")
	}
	limit = queryLimit(limit)
	if offset < 0 {
		return nil, false, invalidf("offset must be non-negative")
	}
	for i, a := range args {
		args[i] = normalizeArg(a)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, false, err
	}
	// One read snapshot covers the registry read, validation, and execution:
	// otherwise a concurrent DropTable of a grandfathered pragma_*/dbstat
	// table could commit between them, and SQLite would resolve the now-
	// absent physical table to its built-in eponymous virtual table.
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	registered, err := registeredTables(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if err := validateQueryTables(trimmed, registered); err != nil {
		return nil, false, err
	}
	paginated := trimmed + "\nLIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)

	// Most SELECT/WITH statements accept a trailing LIMIT on a fresh line.
	// Put the LIMIT on its own line so a trailing `--` line comment does not
	// swallow the placeholders. Statements that still reject LIMIT (e.g.
	// VALUES, some compound statements) are transparently wrapped in a
	// subquery on a retry, preserving the original labels and duplicate
	// detection for plain SELECTs.
	rows, err := tx.QueryContext(ctx, paginated, args...)
	if err != nil {
		// A missing table fails the wrapped form the same way, so only retry
		// statements that reject a trailing LIMIT (VALUES, some compounds) in
		// a subquery. When the retry also fails the statement is invalid on
		// its own — a statement that merely rejects a trailing LIMIT succeeds
		// via the retry — so re-classify against the caller's statement as
		// written: it fails at prepare time, before any rows are read.
		first := err
		userArgs := args[:len(args)-2]
		if !strings.Contains(first.Error(), "no such table") {
			wrapped := "SELECT * FROM (\n" + trimmed + "\n)\nLIMIT ? OFFSET ?"
			rows, err = tx.QueryContext(ctx, wrapped, args...)
			if err == nil {
				// SQLite disambiguates duplicate labels in a subquery (a, a:1),
				// which would silently rename keys the unwrapped form rejects
				// as duplicates. Validate the caller's own labels — prepare-time
				// metadata, no rows are read — so the duplicate-label contract
				// survives the retry.
				if probe, perr := tx.QueryContext(ctx, trimmed, userArgs...); perr == nil {
					if cols, cerr := probe.Columns(); cerr == nil {
						seen := make(map[string]bool, len(cols))
						for _, c := range cols {
							if seen[c] {
								probe.Close()
								return nil, false, invalidf("duplicate column label %q in query result; use AS aliases", c)
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
			return nil, false, NewQueryError(trimmed, err)
		}
	}
	defer rows.Close()

	proj, err := s.nsProjection(ctx, tx, paginated)
	if err != nil {
		return nil, false, err
	}
	return rowsToMaps(rows, proj, limit)
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
			// We fetched the (limit+1)th row, so there are more rows available.
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

func rawValSize(v any) int {
	switch t := v.(type) {
	case []byte:
		return len(t)
	case string:
		return len(t)
	default:
		return 16
	}
}

func approxSize(v any) int {
	switch t := v.(type) {
	case string:
		return encodedSize(t)
	case []byte:
		return len(t)
	default:
		return 16
	}
}

func encodedSize(s string) int {
	n := len(s)
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] < 0x20:
			n += 6
		case s[i] == '"' || s[i] == '\\':
			n += 3
		}
	}
	if strings.Contains(s, " ") || strings.Contains(s, " ") {
		n += 4 * (strings.Count(s, " ") + strings.Count(s, " "))
	}
	if !utf8.ValidString(s) {
		for _, r := range s {
			if r == utf8.RuneError {
				n += 6
			}
		}
	}
	return n
}

func normalizeVal(v any) any {
	if b, ok := v.([]byte); ok {
		return base64.StdEncoding.EncodeToString(b)
	}
	return v
}
