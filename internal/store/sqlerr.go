package store

import (
	"fmt"
	"regexp"
	"strings"
)

// QueryError is a sanitized, client-safe SQL execution error.
// It wraps either ErrInvalid or ErrNotFound so callers can still use errors.Is,
// while keeping the original SQLite error available for logging.
type QueryError struct {
	msg      string
	sentinel error
	cause    error
}

func (e *QueryError) Error() string { return e.msg }
func (e *QueryError) Unwrap() error { return e.sentinel }

// Cause returns the original, unsanitized SQLite error for diagnostics.
func (e *QueryError) Cause() error { return e.cause }

var (
	// SQLite error patterns. These are intentionally conservative: they only
	// match well-known SQLite message shapes so we do not accidentally mangle
	// hand-written Dolmen error strings.
	sqlLogicErrorRe = regexp.MustCompile(`(?i)SQL\s+logic\s+error:\s*`)
	lineNumberRe    = regexp.MustCompile(`\s+\(\d+\)\s*$`)
	nearRe          = regexp.MustCompile(`(?i)near\s+["']([^"']+)["']\s*:\s*syntax\s+error`)
	unrecognizedRe  = regexp.MustCompile(`(?i)unrecognized\s+token:\s*["']([^"']+)["']`)
	noSuchTableRe   = regexp.MustCompile(`(?i)no\s+such\s+table:\s*(\S+)`)
	noSuchColumnRe  = regexp.MustCompile(`(?i)no\s+such\s+column:\s*(\S+)`)
	noSuchFuncRe    = regexp.MustCompile(`(?i)no\s+such\s+function:\s*(\S+)`)
	missingArgRe    = regexp.MustCompile(`(?i)missing\s+argument\s+with\s+index\s+(\d+)`)
	incompleteRe    = regexp.MustCompile(`(?i)\bincomplete\s+input\b`)
	malformedJSONRe = regexp.MustCompile(`(?i)\bmalformed\s+JSON\b`)
	tooManyVarsRe   = regexp.MustCompile(`(?i)too\s+many\s+SQL\s+variables`)
	misuseRe        = regexp.MustCompile(`(?i)misuse\s+at.*`)
)

// NewQueryError sanitizes a raw SQLite error for the query endpoint.
// It returns a QueryError that preserves the ErrInvalid/ErrNotFound sentinel
// while keeping the original error for server-side logging.
func NewQueryError(sql string, err error) error {
	base := RedactSQLMessage(err.Error())
	sentinel := ErrInvalid
	hint := ""

	switch {
	case strings.HasPrefix(base, "table "):
		sentinel = ErrNotFound
		hint = "use list_tables to see available tables"
	case strings.HasPrefix(base, "column "):
		hint = "use describe_table to see valid column names"
	case strings.HasPrefix(base, "SQL syntax error near "), strings.HasPrefix(base, "unrecognized SQL token "), base == "incomplete SQL statement":
		hint = "only single SELECT or WITH statements are allowed; use ? for parameters and table/column names from list_tables or describe_table"
	case strings.HasPrefix(base, "missing value for query parameter"):
		hint = "pass the missing value in args"
	case strings.HasPrefix(base, "too many query parameters"):
		hint = "the limit is 100 query parameters"
	case strings.HasPrefix(base, "unknown SQL function "):
		hint = "only standard SQL functions and table/column names from describe_table are supported"
	default:
		hint = "the query endpoint only accepts a single SELECT or WITH statement; use ? for parameters"
	}

	_ = sql // reserved for future structured diagnostics; not echoed to avoid leaking input
	msg := base
	if hint != "" {
		msg = base + "; " + hint
	}
	return &QueryError{msg: msg, sentinel: sentinel, cause: err}
}

// redactSQLMessage turns a raw SQLite error string into a client-safe string.
// It extracts the offending token where one is present and removes SQLite
// internal framing such as "SQL logic error:" and trailing line numbers.
func RedactSQLMessage(raw string) string {
	msg := strings.TrimSpace(raw)
	msg = sqlLogicErrorRe.ReplaceAllString(msg, "")
	msg = strings.TrimSpace(msg)
	msg = lineNumberRe.ReplaceAllString(msg, "")
	msg = strings.TrimSpace(msg)

	if m := nearRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("SQL syntax error near %q", m[1])
	}
	if m := unrecognizedRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("unrecognized SQL token %q", m[1])
	}
	if m := noSuchTableRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("table %q not found", m[1])
	}
	if m := noSuchColumnRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("column %q not found", m[1])
	}
	if m := noSuchFuncRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("unknown SQL function %q", m[1])
	}
	if m := missingArgRe.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("missing value for query parameter ?%s", m[1])
	}
	if incompleteRe.MatchString(msg) {
		return "incomplete SQL statement"
	}
	if malformedJSONRe.MatchString(msg) {
		return "malformed JSON in SQL expression"
	}
	if tooManyVarsRe.MatchString(msg) {
		return "too many query parameters"
	}
	if misuseRe.MatchString(msg) {
		return "invalid use of SQL"
	}
	if strings.Contains(msg, "SQLITE_") || strings.Contains(msg, "SQL logic") {
		return "the SQL could not be executed"
	}
	return msg
}
