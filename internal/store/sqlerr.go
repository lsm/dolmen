package store

import (
	"fmt"
	"regexp"
	"strings"
)

type QueryError struct {
	msg      string
	sentinel error
	cause    error
}

func (e *QueryError) Error() string { return e.msg }

func (e *QueryError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.sentinel}
	}
	return []error{e.cause, e.sentinel}
}

func (e *QueryError) Cause() error { return e.cause }

var (
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

func NewQueryError(sql string, err error) error {
	return newSQLExecError(sql, err, false)
}

func NewFilterError(where string, err error) error {
	return newSQLExecError(where, err, true)
}

func newSQLExecError(sql string, err error, filter bool) error {
	if !recognizedQueryError(err.Error()) {
		return err
	}
	base := RedactSQLMessage(err.Error())
	sentinel := ErrInvalid
	syntaxHint := "only single SELECT or WITH statements are allowed; use ? for parameters and table/column names from list_tables or describe_table"
	defaultHint := "the query endpoint only accepts a single SELECT or WITH statement; use ? for parameters"
	if filter {
		syntaxHint = `the filter must be a single SQL WHERE expression (e.g. "status = 'done'" or "id IN (3, 7)"); use ? for parameters and column names from describe_table`
		defaultHint = "the filter must be a valid SQL WHERE expression; use ? for parameters"
	}
	hint := ""

	switch {
	case strings.HasPrefix(base, "table "):
		sentinel = ErrNotFound
		hint = "use list_tables to see available tables"
	case strings.HasPrefix(base, "column "):
		hint = "use describe_table to see valid column names"
	case strings.HasPrefix(base, "SQL syntax error near "), strings.HasPrefix(base, "unrecognized SQL token "), base == "incomplete SQL statement":
		hint = syntaxHint
	case strings.HasPrefix(base, "missing value for query parameter"):
		hint = "pass the missing value in args"
	case strings.HasPrefix(base, "too many query parameters"):
		hint = "the limit is 100 query parameters"
	case strings.HasPrefix(base, "unknown SQL function "):
		hint = "only standard SQL functions and table/column names from describe_table are supported"
	default:
		hint = defaultHint
	}

	_ = sql
	msg := base
	if hint != "" {
		msg = base + "; " + hint
	}
	return &QueryError{msg: msg, sentinel: sentinel, cause: err}
}

var operationalSQLiteRe = regexp.MustCompile(`(?i)\bSQLITE_(BUSY|LOCKED|CORRUPT|IOERR|FULL|NOMEM|READONLY|CANTOPEN|INTERRUPT|PROTOCOL)\b|\b(?:database is locked|database is busy|disk I/O error|database disk image is malformed|out of memory|database or disk is full|attempt to write a readonly database|unable to open database file)\b`)

func recognizedQueryError(raw string) bool {
	if operationalSQLiteRe.MatchString(raw) {
		return false
	}
	for _, re := range []*regexp.Regexp{
		nearRe, unrecognizedRe, noSuchTableRe, noSuchColumnRe, noSuchFuncRe,
		missingArgRe, incompleteRe, malformedJSONRe, tooManyVarsRe, misuseRe,
	} {
		if re.MatchString(raw) {
			return true
		}
	}
	trimmed := strings.TrimSpace(raw)
	return sqlLogicErrorRe.MatchString(trimmed) || strings.HasSuffix(trimmed, "(1)")
}

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

type RedactedSQLite struct {
	msg   string
	cause error
}

func (e *RedactedSQLite) Error() string { return e.msg }

func (e *RedactedSQLite) Unwrap() error { return e.cause }

func (e *RedactedSQLite) Cause() error { return e.cause }

func NewRedactedSQLite(err error) error {
	return &RedactedSQLite{msg: RedactSQLMessage(err.Error()), cause: err}
}
