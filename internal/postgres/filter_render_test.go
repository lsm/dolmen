package postgres

import (
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/filter"
	"github.com/lsm/dolmen/internal/schema"
)

func renderFor(t *testing.T, expr string, args ...any) string {
	t.Helper()
	node, err := filter.Parse(expr, filter.Options{Columns: []string{"id", "body", "created_at"}, Args: args})
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	sql, _, err := renderScopedFilter(node,
		map[string]string{"id": "id", "body": "c1", "created_at": "created_at"},
		map[string]schema.FieldType{"id": schema.Number, "body": schema.Text, "created_at": schema.Timestamp},
		args, 1)
	if err != nil {
		t.Fatalf("render(%q): %v", expr, err)
	}
	return sql
}

func TestNoCallerTextReachesTheStatementUnquoted(t *testing.T) {
	for _, text := range []string{`a`, `a''b`, `a\`, `a\\`, `%`, `_`, `;DROP`, `$dolmen$`, `$dolmen0$`} {
		sql := renderFor(t, "body = '"+strings.ReplaceAll(text, "'", "''")+"'")
		quoted := dollarQuote(text)
		if !strings.Contains(sql, quoted) {
			t.Fatalf("render of %q did not carry it in a dollar quote (%s); an ordinary quoted literal ending in a backslash runs on into the statement when standard_conforming_strings is off", text, sql)
		}
		if strings.Count(sql, quoted[:strings.Index(quoted[1:], "$")+2]) != 2 {
			t.Fatalf("render of %q chose a tag that appears in its own text: %s", text, sql)
		}
		if !strings.HasSuffix(sql, ")") {
			t.Fatalf("render of %q did not close its expression: %s", text, sql)
		}
	}
}

func TestADollarQuotedLiteralCarriesItsTextExactly(t *testing.T) {
	sql := renderFor(t, `body = 'a\'`)
	if !strings.Contains(sql, `a\`) {
		t.Fatalf("the literal lost its trailing backslash: %s", sql)
	}
	open := strings.Index(sql, "$")
	if open < 0 {
		t.Fatalf("no dollar quote in %s", sql)
	}
	tag := sql[open : strings.Index(sql[open+1:], "$")+open+2]
	if strings.Count(sql, tag) != 2 {
		t.Fatalf("the dollar quote %q does not open and close exactly once in %s", tag, sql)
	}
}

func TestABlobLiteralIsNotAnEscapeSequence(t *testing.T) {
	sql := renderFor(t, `body = X'6162'`)
	if strings.Contains(sql, `'\x`) {
		t.Fatalf("a bytea literal written with an ordinary quote loses its backslash when standard_conforming_strings is off: %s", sql)
	}
}

func TestAHexLiteralCarriesSQLitesTwosComplementValue(t *testing.T) {
	for _, tc := range []struct{ expr, want string }{
		{"0x1f", "31"},
		{"0x7fffffffffffffff", "9223372036854775807"},
		{"0xffffffffffffffff", "-1"},
	} {
		sql := renderFor(t, "id = "+tc.expr)
		if !strings.Contains(sql, tc.want) {
			t.Fatalf("render of %s is %s, want the value %s that SQLite reads it as", tc.expr, sql, tc.want)
		}
	}
}
