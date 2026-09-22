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
	{"an integer beyond a double keeps its digits", "n = '-7'", ""},
	{"concatenation is a truth value through its number", "NOT (body || body)", ""},
	{"numeric concatenation is a true truth value", "code || ''", ""},
	{"a text case expression is a truth value", "NOT (CASE WHEN 1 = 1 THEN 'a' ELSE 'b' END)", ""},
	{"a text iif is a truth value", "NOT iif(1 = 1, 'a', 'b')", ""},
	{"a text coalesce is a truth value", "NOT coalesce(body, 'x')", ""},
}

var notYetEvaluatedByAdapterTwo = map[string]bool{
	"date": true, "time": true, "datetime": true, "julianday": true, "strftime": true,
	"date with a modifier": true, "time with a modifier": true, "datetime with a modifier": true,
	"julianday with a modifier": true, "strftime with a modifier": true,
	"datetime with two modifiers": true, "strftime with two modifiers": true,
	"substr from a negative start": true, "substr with a negative length": true,
	"coalesce over mixed types": true,
	"round to a negative place": true,
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
		},
		"row_access": "own",
	})
	grantTo(t, h, "principal", "alice", "acme", "notes", "create", "delete")
	res, out := h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"a note from alice","n":-7,"code":"1","mark":"!zzz","huge":"1e999999","flag":true,"neg":"-1.0e+300","frac":0.1}]}`)
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
