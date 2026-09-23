package conformance

import (
	"net/http"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

var sharedFilterList = []struct {
	function string
	filter   string
}{
	{"abs", "abs(0 - id) = id"},
	{"round", "round(1.6) = 2"},
	{"length", "length(body) = 17"},
	{"lower", "lower('A NOTE FROM ALICE') = body"},
	{"upper", "upper(body) = 'A NOTE FROM ALICE'"},
	{"substr", "substr(body, 3, 4) = 'note'"},
	{"trim", "trim('  ' || body || '  ') = body"},
	{"trim with a strip set", "trim(body, 'ae') = ' note from alic'"},
	{"ltrim", "ltrim(body, 'a') = ' note from alice'"},
	{"rtrim", "rtrim(body, 'e') = 'a note from alic'"},
	{"replace", "replace(body, 'alice', 'bob') = 'a note from bob'"},
	{"instr", "instr(body, 'note') = 3"},
	{"coalesce", "coalesce(body, 'x') = body"},
	{"coalesce past a null", "coalesce(NULL, body) = body"},
	{"coalesce over mixed types", "coalesce(body, 1) = body"},
	{"ifnull", "ifnull(body, 'x') = body"},
	{"ifnull on a null", "ifnull(NULL, body) = body"},
	{"nullif", "nullif(body, 'x') = body"},
	{"nullif on equal arguments", "nullif(body, body) IS NULL"},
	{"iif", "iif(id = 1, 'y', 'n') = 'y'"},
	{"iif on a false condition", "iif(id = 2, 'y', 'n') = 'n'"},
	{"date", "length(date(created_at)) = 10"},
	{"time", "length(time(created_at)) = 8"},
	{"datetime", "length(datetime(created_at)) = 19"},
	{"julianday", "julianday(created_at) > 2400000"},
	{"strftime", "length(strftime('%Y', created_at)) = 4"},
	{"date with a modifier", "date(created_at, '+1 day') > date(created_at)"},
	{"time with a modifier", "time(created_at, '+1 hour') <> time(created_at)"},
	{"datetime with a modifier", "datetime(created_at, '+1 day') > datetime(created_at)"},
	{"julianday with a modifier", "julianday(created_at, '+1 day') > julianday(created_at)"},
	{"strftime with a modifier", "strftime('%Y-%m-%d', created_at, '+1 day') > strftime('%Y-%m-%d', created_at)"},
	{"round to a given place", "round(1.234, 1) = 1.2"},
	{"round to a negative place", "round(123.4, -1) = 123"},
	{"substr without a length", "substr(body, 3) = 'note from alice'"},
	{"substr from a negative start", "substr(body, -5) = 'alice'"},
	{"substr with a negative length", "substr(body, 3, -1) = ' '"},
	{"coalesce beyond two arguments", "coalesce(NULL, NULL, 'x') = 'x'"},
	{"datetime with two modifiers", "datetime(created_at, '+1 day', '+1 hour') > datetime(created_at, '+1 day')"},
	{"strftime with two modifiers", "strftime('%Y-%m-%d', created_at, '+1 day', '+1 day') > strftime('%Y-%m-%d', created_at, '+1 day')"},
}

var allowlistedOperators = []struct {
	spelling string
	filter   string
	args     string
}{
	{"=", "id = 1", ""},
	{"== as a spelling of =", "id == 1", ""},
	{"!=", "id != 2", ""},
	{"<> as a spelling of !=", "id <> 2", ""},
	{"relational comparison", "id >= 1 AND id <= 1", ""},
	{"arithmetic", "id + 1 = 2 AND id * 3 = 3 AND id - 1 = 0", ""},
	{"modulo", "7 % 2 = 1", ""},
	{"concatenation", "body || '!' = 'a note from alice!'", ""},
	{"AND, OR and NOT", "NOT (id = 2) AND (id = 1 OR id = 3)", ""},
	{"IS against a literal", "body IS 'a note from alice'", ""},
	{"IS against a bound argument", "body IS ?", `["a note from alice"]`},
	{"IS NOT", "body IS NOT NULL", ""},
	{"IN over a literal list", "id IN (1, 2, 3)", ""},
	{"NOT IN", "id NOT IN (2, 3)", ""},
	{"NOT IN over an empty list", "id NOT IN ()", ""},
	{"IN over bound arguments", "id IN (?, ?)", "[1, 2]"},
	{"BETWEEN", "id BETWEEN 1 AND 10", ""},
	{"NOT BETWEEN", "id NOT BETWEEN 2 AND 10", ""},
	{"LIKE with ESCAPE", "('100%' LIKE '100!%' ESCAPE '!') AND NOT ('100X' LIKE '100!%' ESCAPE '!')", ""},
	{"LIKE with ESCAPE over an underscore", "('a_b' LIKE 'a!_b' ESCAPE '!') AND NOT ('axb' LIKE 'a!_b' ESCAPE '!')", ""},
	{"LIKE with a bound ESCAPE", "'100%' LIKE '100!%' ESCAPE ?", `["!"]`},
	{"NOT LIKE", "body NOT LIKE 'zzz%'", ""},
	{"CASE", "CASE WHEN id = 1 THEN 1 ELSE 0 END = 1", ""},
	{"a bound argument", "id = ?", "[1]"},
}

var pinnedFilterSemantics = []struct {
	rule   string
	filter string
	args   string
}{
	{"LIKE is ASCII-case-insensitive", "body LIKE 'A NOTE%'", ""},
	{"string comparison is BINARY byte-wise", "'a' > 'B'", ""},
	{"integer division truncates toward zero", "-7 / 2 = -3", ""},
	{"integer division truncates a stored number too", "n / 2 = -3", ""},
	{"a numeric function coerces nonnumeric text to zero", "abs(body) = 0", ""},
	{"modulo converts fractional operands to integers", "9.2 % 2.9 = 1", ""},
	{"lower case-maps ASCII only", "lower('Æ') = 'Æ'", ""},
	{"upper case-maps ASCII only", "upper('æ') = 'æ'", ""},
	{"division by zero is null", "(1 / 0) IS NULL", ""},
	{"modulo by zero is null", "(1 % 0) IS NULL", ""},
	{"round goes half away from zero", "round(-2.5) = -3", ""},
	{"a coerced numeric function keeps its later arguments", "round('1.567', 1) = 1.6", ""},
	{"nonnumeric text coerces to zero in arithmetic", "body + 1 = 1", ""},
	{"numeric text coerces to a number in arithmetic", "'3' + 1 = 4", ""},
	{"concatenation coerces numbers to text", "1 || '2' = '12'", ""},
	{"null propagates through comparison", "(NULL = 1) IS NULL", ""},
	{"null is not distinct from null under IS", "NULL IS NULL", ""},
	{"LIKE folding stops at ASCII", "'æ' NOT LIKE 'Æ'", ""},
	{"a blob literal is bytes, not bits", "length(X'6162') = 2", ""},
	{"a hexadecimal integer literal", "0x1f = 31", ""},
	{"a scalar is a truth value", "1", ""},
	{"numeric-looking text still is not a number", "NOT ('3' = 3)", ""},
	{"text never equals a number", "NOT (body = 1)", ""},
	{"text sorts after a number", "body > 1", ""},
	{"a text column takes the text form of the number it is compared to", "code = 1", ""},
	{"a text column below the digits does not sort after a number", "NOT (mark > 1)", ""},
	{"a number column converts numeric text before comparing", "n = '-7'", ""},
	{"a number column leaves nonnumeric text as text", "NOT (n > 'abc')", ""},
	{"a numeral too large for a double saturates to infinity", "abs(huge) > 1", ""},
	{"an overflowing literal saturates rather than failing", "abs(1e999999) > 1", ""},
	{"a number column saturates overflowing numeric text", "n < '1e999999'", ""},
	{"a text column left of a number column keeps its own class order", "NOT (body < n)", ""},
	{"a text column right of a number column keeps its own class order", "n < body", ""},
	{"a bound numeric string takes a number column's affinity", "n = ?", `["-7"]`},
	{"a bound nonnumeric string stays text against a number column", "NOT (n > ?)", `["abc"]`},
	{"a bound numeric string skips every space SQLite skips", "n = ?", `["\u000b-7\u000b"]`},
	{"a bound numeral behind a unicode space is not numeric text", "(n % ?) IS NULL", `["\u00a0-7"]`},
	{"a unicode space is not a space SQLite skips", "NOT (n = ?)", `["\u2002-7"]`},
	{"a null bound argument is not distinct from a null column", "absent IS ?", `[null]`},
	{"a bound boolean is not distinct from a boolean column", "flag IS ?", `[true]`},
	{"a bound fraction stays fractional as a truth value", "?", `[0.1]`},
	{"a bound fraction stays fractional against an integer", "? > 0", `[0.1]`},
	{"a bound text argument is its own truth value", "?", `["1"]`},
	{"nonnumeric bound text is false as a truth value", "NOT ?", `["abc"]`},
	{"a bound boolean inside LIKE is its digit", "code LIKE ?", `[true]`},
	{"round always yields a real, so its quotient is not truncated", "round(n) / 2 = -3.5", ""},
	{"a rounded integer does not divide as an integer", "NOT (round(n) / 2 = -3)", ""},
	{"abs over text yields a real", "abs(code) / 2 = 0.5", ""},
	{"abs over an integer stays an integer", "abs(n) / 2 = 3", ""},
	{"round clamps a negative place to none", "round(1234.5678, -2) = 1235", ""},
	{"round truncates its place toward zero", "round(2.567, 1.9) = 2.6", ""},
	{"a null place makes the rounding null", "round(2.567, NULL) IS NULL", ""},
	{"a place past a signed integer still rounds", "round(1.5, 1e19) = 1.5", ""},
	{"a place past every digit a double has still rounds", "round(n, 2147483648) = -7", ""},
	{"round loses what a double cannot hold", "NOT (big = round(big))", ""},
	{"a rounded integer past a double is the double", "round(big) = 9007199254740992", ""},
	{"abs over text loses what a double cannot hold", "abs('9007199254740993') = 9007199254740992", ""},
	{"text underflowing a double is zero, not infinite", "NOT (abs(tiny) > 1)", ""},
	{"text underflowing a double coerces to zero", "abs(tiny) = 0", ""},
	{"a stored numeral saturates like the same literal", "abs(tiny) = abs('1e-999999')", ""},
	{"text overflowing a double is still infinite", "abs(huge) > 1", ""},
	{"an underflowing literal is zero too", "abs('1e-999999') = 0", ""},
	{"an overflowing literal is still infinite", "abs('1e999999') > 1", ""},
	{"an underflowing bound argument is zero too", "abs(?) = 0", `["1e-999999"]`},
	{"a numeral just past a double still underflows", "abs('1e-400') = 0", ""},
	{"an integer sum past int64 becomes a double", "9223372036854775807 + 2 = 9223372036854775808", ""},
	{"an integer product past int64 becomes a double", "9223372036854775807 * 2 > 9223372036854775807", ""},
	{"int64 min divided by minus one becomes a double", "(-9223372036854775808 / -1) = 9223372036854775808", ""},
	{"arithmetic inside int64 stays exact", "9223372036854775806 + 1 = 9223372036854775807", ""},
	{"a numeral past int64 is a double", "past = 9223372036854775808", ""},
	{"int64 min is still an integer under division", "-9223372036854775808 / 1 = -9223372036854775808", ""},
	{"a sum of doubles divides as a double", "(1.5 + 1.5) / 2 = 1.5", ""},
	{"a whole double quotient still compares as a double", "((1.5 + 1.5) / 2) > 1", ""},
	{"a double product of a stored number divides as a double", "(n * 3.0) / 2 = -10.5", ""},
	{"a chain of doubles stays a double", "((1.5 + 1.5) * 2) / 4 = 1.5", ""},
	{"a negated double divides as a double", "-(1.5 + 1.5) / 2 = -1.5", ""},
	{"the absolute value of a double divides as a double", "abs(1.5 + 1.5) / 2 = 1.5", ""},
	{"a coalesced double divides as a double", "coalesce(round(3), 0) / 2 = 1.5", ""},
	{"a coalesced integer divides as an integer", "coalesce(NULL, 3) / 2 = 1", ""},
	{"a chosen double divides as a double", "iif(1, 3.0, 0) / 2 = 1.5", ""},
	{"a chosen integer divides as an integer", "iif(id = 1, 3, 0.5) / 2 = 1", ""},
	{"a CASE double divides as a double", "CASE WHEN id = 1 THEN 3.0 ELSE 0 END / 2 = 1.5", ""},
	{"a nullif double divides as a double", "nullif(3.0, 0) / 2 = 1.5", ""},
	{"a modulo of a double is a double", "(7.5 % 2) / 2 = 0.5", ""},
	{"a modulo of integers is an integer", "(7 % 4) / 2 = 1", ""},
	{"a constant text expression saturates too", "abs('1e999999' || '') > 1", ""},
	{"a coalesced constant text saturates too", "abs(coalesce('1e-999999', 'x')) = 0", ""},
	{"a boolean expression never equals text", "NOT ((n > 1) = body)", ""},
	{"a boolean expression takes a text column's affinity", "pad < (n > 1)", ""},
	{"a boolean expression is not below every text column", "NOT (pad > (n > 1))", ""},
	{"a boolean expression against a text literal keeps class order", "'0' > (n > 1)", ""},
	{"a boolean expression never equals a text literal", "NOT ('0' = (n > 1))", ""},
	{"a boolean expression concatenates as its digit", "((n > 1) || 'x') = '0x'", ""},
	{"a bound number takes a text column's affinity", "code = ?", `[1]`},
	{"a boolean column is its own truth value", "flag", ""},
	{"a boolean column joins a numeric truth value", "flag AND n", ""},
	{"nonnumeric text is false as a truth value", "NOT body", ""},
	{"numeric text is true as a truth value", "code", ""},
	{"a scalar under NOT is a truth value", "NOT 0", ""},
	{"a scalar under AND is a truth value", "1 AND 1", ""},
	{"a bound boolean is its own truth value", "?", `[true]`},
	{"a negated literal takes the text form SQLite gives it", "neg = -1e300", ""},
	{"numeric text rounds through a double before comparing", "frac = '0.10000000000000000001'", ""},
	{"an integer beyond a double keeps its digits", "big = '9007199254740993'", ""},
	{"a real literal divides as a real", "7.0 / 2 = 3.5", ""},
	{"a real divisor divides as a real", "7 / 2.0 = 3.5", ""},
	{"an exponent literal divides as a real", "1e1 / 4 = 2.5", ""},
	{"real text divides as a real", "real / 2 = 3.5", ""},
	{"integer text still divides as an integer", "code / 2 = 0", ""},
	{"a null text keeps its null through arithmetic", "absent + 1 IS NULL", ""},
	{"a null text keeps its null through a numeric function", "abs(absent) IS NULL", ""},
	{"a null text is not a false truth value", "(NOT absent) IS NULL", ""},
	{"nonnumeric text is still zero rather than null", "body + 1 = 1", ""},
	{"a digit shaped blob is a true truth value", "X'31'", ""},
	{"a letter shaped blob is a false truth value", "NOT X'6162'", ""},
	{"a false boolean column is a false truth value", "NOT off", ""},
	{"a false boolean column is the integer zero", "off = 0", ""},
	{"a zero number is a false truth value", "NOT zero", ""},
	{"empty text is a false truth value", "NOT empty", ""},
	{"empty text coerces to zero rather than null", "empty + 1 = 1", ""},
	{"a numeral head tolerates leading spaces", "pad + 1 = 6", ""},
	{"text affinity keeps the spaces the numeral head skipped", "NOT (pad = 5)", ""},
	{"LIKE takes a number as its text", "1 LIKE '1'", ""},
	{"LIKE compares a text column against a number's text", "code LIKE 1", ""},
	{"LIKE does not match a note against a digit", "NOT (body LIKE 1)", ""},
	{"a case operand converts to the branch's affinity", "CASE code WHEN 1 THEN 1 ELSE 0 END = 1", ""},
	{"a numeral past int64 divides as a real", "NOT ('9999999999999999998' / 3 = 3333333333333333332)", ""},
	{"a stored number past int64 divides as a real", "NOT (past / 3 = 3074457345618258602)", ""},
	{"int64 min is still an integer", "1 / -9223372036854775808 = 0", ""},
	{"a negated zero has no sign in its text", "'0' LIKE -0", ""},
	{"modulo reads a text numeral as its integer prefix", "1 % '1e400' = 0", ""},
	{"infinity divided by infinity is null", "('1e400' / '1e400') IS NULL", ""},
	{"a blob concatenates as its bytes", "X'31' || 'x' = '1x'", ""},
	{"a null survives the integer clamp", "(absent % 2) IS NULL", ""},
	{"real arithmetic carries the double's rounding", "NOT (0.1 + 0.2 = 0.3)", ""},
	{"the rounding leans the way a double leans", "0.1 + 0.2 > 0.3", ""},
	{"a double round trip returns to where it started", "1.0 / 3 * 3 = 1", ""},
	{"a stored real rounds like a double too", "NOT (frac + 0.2 = 0.3)", ""},
	{"integer arithmetic stays exact past a double", "big + 0 = 9007199254740993", ""},
	{"two numerals a double cannot tell apart divide to one", "(9223372036854775807 / 9223372036854775808) = 1", ""},
	{"runtime numeric text rounds through a double too", "frac = code2", ""},
	{"a boolean expression is one in a numeric position", "(flag OR off) = 1", ""},
	{"a false boolean expression is zero", "(flag AND off) = 0", ""},
	{"a comparison is a number when compared to one", "(n = -7) = 1", ""},
	{"a boolean expression converts on either side", "1 = (flag OR off)", ""},
	{"a boolean expression multiplies as its integer", "(flag OR off) * 5 = 5", ""},
	{"a boolean column inside LIKE is its digit", "1 LIKE flag", ""},
	{"LIKE has no escape character of its own", `NOT ('a_b' LIKE 'a\\_b')`, ""},
	{"a backslash in a pattern is an ordinary character", `'a\\Xb' LIKE 'a\\_b'`, ""},
	{"an explicit escape still escapes", "'100%' LIKE '100!%' ESCAPE '!'", ""},
	{"integer text keeps its digits through a comparison", "big = '9007199254740993'", ""},
	{"a case operand still misses a different value", "CASE code WHEN 2 THEN 1 ELSE 0 END = 0", ""},
	{"concatenation is a truth value through its number", "NOT (body || body)", ""},
	{"numeric concatenation is a true truth value", "code || ''", ""},
	{"a text case expression is a truth value", "NOT (CASE WHEN 1 = 1 THEN 'a' ELSE 'b' END)", ""},
	{"a text iif is a truth value", "NOT iif(1 = 1, 'a', 'b')", ""},
	{"a text coalesce is a truth value", "NOT coalesce(body, 'x')", ""},
	{"a boolean column compares as the integer SQLite stores", "flag = 1", ""},
	{"a boolean column adds as an integer", "flag + 1 = 2", ""},
	{"a boolean column passes through a numeric function", "abs(flag) = 1", ""},
	{"a boolean column concatenates as its digit", "flag || 'x' = '1x'", ""},
	{"a boolean column drives iif", "iif(flag, 'a', 'b') = 'a'", ""},
	{"a boolean column drives a case", "CASE WHEN flag THEN 1 ELSE 0 END = 1", ""},
	{"IN converts the list to the column's affinity", "code IN (1, 2)", ""},
	{"BETWEEN converts its bounds to the column's affinity", "code BETWEEN 1 AND 10", ""},
	{"IS converts to the column's affinity", "code IS 1", ""},
	{"IS is still null-safe", "nullif(body, body) IS NULL", ""},
	{"a scalar drives iif", "iif(1, 'a', 'b') = 'a'", ""},
}

var notYetEvaluatedByAdapterTwo = map[string]bool{
	"date": true, "time": true, "datetime": true, "julianday": true, "strftime": true,
	"date with a modifier": true, "time with a modifier": true, "datetime with a modifier": true,
	"julianday with a modifier": true, "strftime with a modifier": true,
	"datetime with two modifiers": true, "strftime with two modifiers": true,
	"substr from a negative start": true, "substr with a negative length": true,
	"coalesce over mixed types": true,
}

var notYetSpelledByAdapterTwo = map[string]bool{}

func seedScopedFilterRow(t *testing.T) *harness {
	t.Helper()
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme",
		"table":     "notes",
		"fields": []map[string]any{
			{"name": "body", "type": "text", "fulltext": true},
			{"name": "n", "type": "number"},
			{"name": "code", "type": "text"},
			{"name": "mark", "type": "text"},
			{"name": "huge", "type": "text"},
			{"name": "flag", "type": "boolean"},
			{"name": "neg", "type": "text"},
			{"name": "frac", "type": "number"},
			{"name": "big", "type": "number"},
			{"name": "real", "type": "text"},
			{"name": "absent", "type": "text"},
			{"name": "off", "type": "boolean"},
			{"name": "zero", "type": "number"},
			{"name": "pad", "type": "text"},
			{"name": "empty", "type": "text"},
			{"name": "past", "type": "number"},
			{"name": "code2", "type": "text"},
			{"name": "tiny", "type": "text"},
		},
		"row_access": "own",
	})
	grantTo(t, h, "principal", "alice", "acme", "notes", "create", "delete")
	res, out := h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"a note from alice","n":-7,"code":"1","mark":"!zzz","huge":"1e999999","flag":true,"neg":"-1.0e+300","frac":0.1,"big":9007199254740993,"real":"7.0","off":false,"zero":0,"pad":"  5  ","empty":"","past":9223372036854775808,"code2":"0.10000000000000000001","tiny":"1e-999999"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("seed insert: status %d %v", res.StatusCode, out)
	}
	return h
}

func mustMatchTheOwnRow(t *testing.T, filter string) {
	t.Helper()
	mustMatchTheOwnRowWithArgs(t, filter, "")
}

func mustMatchTheOwnRowWithArgs(t *testing.T, filter, args string) {
	t.Helper()
	h := seedScopedFilterRow(t)
	body := `{"namespace":"acme","table":"notes","filter":"` + filter + `","dry_run":true`
	if args != "" {
		body += `,"args":` + args
	}
	body += `}`
	res, out := h.asIdentity(t, "alice", "", "delete", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the API layer accepted this filter from the shared allowlist, so the engine owes it an evaluation rather than a refusal: %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if matched, _ := data["matched"].(float64); matched != 1 {
		t.Fatalf("the filter is true of the caller's one row, so it must match it: matched %v: %v", data["matched"], out)
	}
}

func TestEveryEngineEvaluatesTheSharedFilterList(t *testing.T) {
	for _, tc := range sharedFilterList {
		t.Run(tc.function, func(t *testing.T) {
			if testEngine(t) == store.EnginePostgres && notYetEvaluatedByAdapterTwo[tc.function] {
				t.Skipf("adapter #2 validates %s through the shared allowlist but has no evaluation for it; see #388", tc.function)
			}
			mustMatchTheOwnRow(t, tc.filter)
		})
	}
}

func TestEveryEngineAcceptsTheAllowlistedOperatorSpellings(t *testing.T) {
	for _, tc := range allowlistedOperators {
		t.Run(tc.spelling, func(t *testing.T) {
			if testEngine(t) == store.EnginePostgres && notYetSpelledByAdapterTwo[tc.spelling] {
				t.Skipf("adapter #2 has no evaluation for this spelling; see #388")
			}
			mustMatchTheOwnRowWithArgs(t, tc.filter, tc.args)
		})
	}
}

func TestEveryEngineEvaluatesAScopedFilterWithSQLitesSemantics(t *testing.T) {
	for _, tc := range pinnedFilterSemantics {
		t.Run(tc.rule, func(t *testing.T) {
			mustMatchTheOwnRowWithArgs(t, tc.filter, tc.args)
		})
	}
}
