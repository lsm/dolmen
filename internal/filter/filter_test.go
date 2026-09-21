package filter

import (
	"strings"
	"testing"
)

var tableColumns = []string{"id", "created_at", "owner", "title", "body", "score", "secret", "due"}

func accept(t *testing.T, expr string, args ...any) {
	t.Helper()
	if err := Validate(expr, Options{Columns: tableColumns, Args: args}); err != nil {
		t.Fatalf("Validate(%q) rejected a filter the allowlist permits: %v", expr, err)
	}
}

func reject(t *testing.T, expr string, wants string, args ...any) {
	t.Helper()
	err := Validate(expr, Options{Columns: tableColumns, Args: args})
	if err == nil {
		t.Fatalf("Validate(%q) accepted an expression outside the allowlist", expr)
	}
	if wants != "" && !strings.Contains(err.Error(), wants) {
		t.Fatalf("Validate(%q) error does not teach %q: %v", expr, wants, err)
	}
}

var acceptedExpressions = []string{
	`1=1`,
	`title = ?`,
	`"title" = ?`,
	`score > 3 AND score <= 10`,
	`score >= -1`,
	`NOT (title = ?)`,
	`title IS NULL`,
	`title IS NOT NULL`,
	`title IS ?`,
	`score IN (1, 2, 3)`,
	`title IN (?, ?)`,
	`score NOT IN (-1, -2)`,
	`score BETWEEN 1 AND 10`,
	`score NOT BETWEEN 1 AND 10`,
	`title LIKE ?`,
	`title NOT LIKE 'draft%'`,
	`title LIKE '100!%' ESCAPE '!'`,
	`title || body = ?`,
	`score + 1 * 2 - 3 / 4 % 5 > 0`,
	`abs(score) = 1`,
	`round(score, 2) = 1.5`,
	`length(title) > 0`,
	`lower(title) = upper(body)`,
	`substr(title, 1, 3) = ?`,
	`trim(title) = ltrim(rtrim(body))`,
	`replace(title, 'a', 'b') = ?`,
	`instr(title, 'x') > 0`,
	`coalesce(title, body) = ?`,
	`ifnull(title, '') = nullif(body, '')`,
	`iif(score > 1, 'high', 'low') = ?`,
	`CASE WHEN score > 1 THEN 'high' ELSE 'low' END = ?`,
	`CASE score WHEN 1 THEN 'one' WHEN 2 THEN 'two' END IS NULL`,
	`date(due) = ?`,
	`time(due) = ?`,
	`datetime(due, '+1 day') = ?`,
	`julianday(due) > julianday(?)`,
	`strftime('%Y', due) = ?`,
	`strftime('%s', due, 'start of month', '+1 month', '-1 day') > ?`,
	`TRUE`,
	`score = 0x1f`,
	`body = X'53514C697465'`,
	`score = 1e3`,
	`created_at > ?`,
	`owner = ?`,
}

func TestTheAllowlistIsAccepted(t *testing.T) {
	for _, expr := range acceptedExpressions {
		args := make([]any, strings.Count(expr, "?"))
		for i := range args {
			args[i] = "a-bound-value"
		}
		accept(t, expr, args...)
	}
}

func TestRowLocalOraclesStayTheBoundarysJob(t *testing.T) {
	accept(t, `iif(secret = ?, abs(-9223372036854775808), 1) = 1`, "guess")
	accept(t, `abs(-9223372036854775808) = 1`)
}

func TestReadsBeyondTheRowAreRefused(t *testing.T) {
	for _, tc := range []struct{ expr, teaches string }{
		{`EXISTS (SELECT 1 FROM notes WHERE owner <> ?)`, "may not read rows"},
		{`score IN (SELECT score FROM notes)`, "may not read rows"},
		{`(SELECT count(*) FROM notes) > 0`, "may not read rows"},
		{`notes.title = ?`, "may not name a table"},
		{`"notes"."title" = ?`, "may not name a table"},
		{`title = (SELECT title FROM notes_fts)`, "may not read rows"},
		{`1 = 1 UNION SELECT 1`, "may not read rows"},
	} {
		reject(t, tc.expr, tc.teaches, "x")
	}
}

func TestFunctionsOutsideTheListAreRefused(t *testing.T) {
	for _, expr := range []string{
		`random() > 0`,
		`randomblob(4) = ?`,
		`sqlite_version() = ?`,
		`sqlite_source_id() = ?`,
		`count(*) > 0`,
		`sum(score) > 0`,
		`max(score) > 0`,
		`group_concat(title) = ?`,
		`row_number() OVER () = 1`,
		`json_extract(body, '$.a') = ?`,
		`zeroblob(1000) = ?`,
		`printf('%s', title) = ?`,
		`load_extension('x')`,
		`substring(title, 1, 2) = ?`,
		`unixepoch(due) > 0`,
		`changes() > 0`,
		`last_insert_rowid() > 0`,
		`total_changes() > 0`,
		`likelihood(score > 1, 0.5)`,
		`glob('a*', title)`,
	} {
		reject(t, expr, "", "x")
	}
}

func TestOperatorsOutsideTheListAreRefused(t *testing.T) {
	for _, tc := range []struct{ expr, teaches string }{
		{`score & 1 = 1`, "bit operator"},
		{`score | 1 = 1`, "bit operator"},
		{`score << 1 = 2`, "bit operator"},
		{`score >> 1 = 0`, "bit operator"},
		{`~score = 1`, "bit operator"},
		{`body -> '$.a' = ?`, "json"},
		{`body ->> '$.a' = ?`, "json"},
		{`title GLOB 'a*'`, "glob"},
		{`title REGEXP 'a'`, "regexp"},
		{`title MATCH 'a'`, "match"},
		{`title COLLATE NOCASE = ?`, "collate"},
		{`title ISNULL`, "is null"},
		{`title NOTNULL`, "is not null"},
		{`title IS DISTINCT FROM ?`, "is distinct"},
		{`CAST(score AS TEXT) = ?`, "cast"},
		{`RAISE(ABORT, 'x')`, "raise"},
	} {
		reject(t, tc.expr, tc.teaches, "x")
	}
}

func TestClockReadingFormsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		expr    string
		teaches string
		args    []any
	}{
		{expr: `datetime('now') > due`, teaches: "clock"},
		{expr: `datetime('NOW') > due`, teaches: "clock"},
		{expr: `date(due, 'localtime') = ?`, teaches: "time zone", args: []any{"x"}},
		{expr: `date(due, 'utc') = ?`, teaches: "time zone", args: []any{"x"}},
		{expr: `julianday(('now')) > 0`, teaches: "clock"},
		{expr: `strftime('%s', 'now') > ?`, teaches: "clock", args: []any{"x"}},
		{expr: `datetime(?) > due`, teaches: "clock", args: []any{"now"}},
		{expr: `datetime(?) > due`, teaches: "clock", args: []any{"  NOW  "}},
		{expr: `date(due, ?) = ?`, teaches: "time zone", args: []any{"localtime", "x"}},
		{expr: `CURRENT_TIMESTAMP > due`, teaches: "clock"},
		{expr: `CURRENT_DATE > due`, teaches: "clock"},
		{expr: `CURRENT_TIME > due`, teaches: "clock"},
	} {
		reject(t, tc.expr, tc.teaches, tc.args...)
	}
	accept(t, `datetime(?) > due`, "2026-01-01T00:00:00Z")
	accept(t, `datetime(due, '+1 day', 'start of month') = ?`, "x")
}

func TestUnknownColumnsAreRefused(t *testing.T) {
	reject(t, `nope = ?`, "not a column", "x")
	reject(t, `NOPE = ?`, "not a column", "x")
	err := Validate(`nope = ?`, Options{Columns: tableColumns, Args: []any{"x"}})
	if !strings.Contains(err.Error(), "title") {
		t.Fatalf("the error does not name the columns that do exist: %v", err)
	}
}

func TestShapeErrorsAreRefused(t *testing.T) {
	for _, tc := range []struct{ expr, teaches string }{
		{``, "empty"},
		{`   `, "empty"},
		{`title = ?; DROP TABLE notes`, "second statement"},
		{`title = ? -- comment`, "comments"},
		{`title = ? /* comment */`, "comments"},
		{`title = 'unclosed`, "never closed"},
		{`(title = ?`, "closing parenthesis"},
		{`title = ?)`, "left over"},
		{`title = ?1`, "numbered parameters"},
		{`title = :name`, "named parameters"},
		{`title = @name`, "named parameters"},
		{`title = $name`, "named parameters"},
		{"`title` = ?", "backtick"},
		{`[title] = ?`, "bracket"},
		{`title =`, "cannot start a value"},
		{`AND title = ?`, "cannot start a value"},
		{`score BETWEEN 1`, "needs an and"},
		{`CASE WHEN score > 1 THEN 'a'`, "needs an end"},
		{`CASE score END`, "needs at least one when"},
		{`abs()`, "1 argument"},
		{`abs(1, 2)`, "1 argument"},
		{`round(1, 2, 3)`, "arguments"},
		{`title NOT NULL`, "is not null"},
		{`score IN 1`, "parenthesised list"},
		{`score IN (score)`, "only literal values"},
	} {
		reject(t, tc.expr, tc.teaches, "x")
	}
}

func TestArgumentCountMustMatch(t *testing.T) {
	reject(t, `title = ? AND body = ?`, "placeholder", "only-one")
	reject(t, `title = ?`, "placeholder", "one", "two")
	accept(t, `title = ? AND body = ?`, "one", "two")
	accept(t, `score IN (?, ?, ?)`, 1, 2, 3)
}

func TestNoColumnsMeansNoColumnReferences(t *testing.T) {
	if err := Validate(`1 = 1`, Options{}); err != nil {
		t.Fatalf("a constant filter needs no columns: %v", err)
	}
	if err := Validate(`title = 1`, Options{}); err == nil {
		t.Fatal("a column reference was accepted against a table with no columns")
	}
}

func TestDeepNestingIsRefusedRatherThanExhaustingTheStack(t *testing.T) {
	for _, shape := range []struct{ open, close string }{
		{"(", ")"},
		{"NOT ", ""},
		{"- ", ""},
	} {
		deep := strings.Repeat(shape.open, MaxNestingDepth+50) + "1" + strings.Repeat(shape.close, MaxNestingDepth+50)
		err := Validate(deep, Options{Columns: tableColumns})
		if err == nil {
			t.Fatalf("a filter nested %d deep with %q was accepted; the parser recurses, so this is a crash rather than an error", MaxNestingDepth+50, shape.open)
		}
		if !strings.Contains(err.Error(), "deep") {
			t.Fatalf("the refusal does not name the nesting: %v", err)
		}
	}
	shallow := strings.Repeat("(", 100) + "1=1" + strings.Repeat(")", 100)
	accept(t, shallow)
}

func TestAnOverlongFilterIsRefusedBeforeParsing(t *testing.T) {
	long := "title = '" + strings.Repeat("a", MaxFilterLength) + "'"
	err := Validate(long, Options{Columns: tableColumns})
	if err == nil {
		t.Fatal("a filter past the length limit was parsed")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("the refusal does not name the limit: %v", err)
	}
}

func TestAComputedStringCannotReachTheClock(t *testing.T) {
	for _, expr := range []string{
		`datetime('no' || 'w') > due`,
		`datetime(substr('znow', 2)) > due`,
		`datetime(replace('xow', 'x', 'n')) > due`,
		`datetime(lower('NOW')) > due`,
		`datetime(iif(1, 'now', 'x')) > due`,
		`datetime(CASE WHEN 1 THEN 'now' END) > due`,
		`julianday(coalesce(NULL, 'now')) > 0`,
	} {
		reject(t, expr, "date or time function")
	}
	for _, expr := range []string{
		`datetime(due, '+1 day') = ?`,
		`datetime(date(due), '+1 day') = ?`,
		`strftime('%s', due) = ?`,
		`julianday(due) > julianday(?)`,
		`date(NULL) IS NULL`,
		`julianday(2451545) > 0`,
	} {
		args := make([]any, strings.Count(expr, "?"))
		for i := range args {
			args[i] = "2026-01-01"
		}
		accept(t, expr, args...)
	}
}

func TestEscapeMustBeOneCharacter(t *testing.T) {
	reject(t, `title LIKE 'a%' ESCAPE 'ab'`, "one character")
	reject(t, `title LIKE 'a%' ESCAPE ?`, "one-character", "ab")
	reject(t, `title LIKE 'a%' ESCAPE ?`, "one-character", 3)
	accept(t, `title LIKE 'a%' ESCAPE '!'`)
	accept(t, `title LIKE ? ESCAPE ?`, "a%", "!")
}

func TestBlobLiteralsMustBeHexadecimal(t *testing.T) {
	reject(t, `body = X'GG'`, "hexadecimal")
	reject(t, `body = X'414'`, "even number")
	accept(t, `body = X'41'`)
}
