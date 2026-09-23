package postgres

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/filter"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

var timeExpressions = []string{
	"date(created_at)",
	"time(created_at)",
	"datetime(created_at)",
	"julianday(created_at)",
	"strftime('%Y', created_at)",
	"strftime('%Y-%m-%d', created_at)",
	"strftime('%Y-%m-%dT%H:%M:%S', created_at)",
	"strftime('%j', created_at)",
	"strftime('day %d of month %m', created_at)",
	"date(created_at, '+1 day')",
	"time(created_at, '+1 hour')",
	"datetime(created_at, '+1 day')",
	"julianday(created_at, '+1 day')",
	"strftime('%Y-%m-%d', created_at, '+1 day')",
	"datetime(created_at, '+1 day', '+1 hour')",
	"strftime('%Y-%m-%d', created_at, '+1 day', '+1 day')",
	"datetime(created_at, '-90 minutes')",
	"datetime(created_at, '+01:30')",
	"datetime(created_at, '+1.5 days')",
	"datetime(created_at, '-1 second')",
	"datetime(created_at, 'bogus')",
	"date(datetime(created_at))",
	"datetime(date(created_at))",
	"datetime(time(created_at))",
	"datetime(julianday(created_at))",
	"date(date(created_at), '+1 day')",
	"datetime(datetime(created_at))",
	"julianday(datetime(created_at))",
	"julianday(date(created_at))",
	"time(datetime(created_at))",
	"julianday(time(created_at))",
	"date('2026-01-31')",
	"date('2026-02-30')",
	"date('2026-02-32')",
	"date('2026-09-99')",
	"datetime('2026-09-21  14:05:09')",
	"datetime('2026-09-21TT14:05:09')",
	"datetime('2026-09-2114:05:09')",
	"datetime('2460000.5')",
	"datetime(2460000.5)",
	"date('9999-12-31', '+1 day')",
	"datetime('9999-12-31 23:59:00', '+1 hour')",
	"date(-1)",
	"datetime(-1)",
	"date(-0.5)",
	"date(+1721426)",
	"date('5373485')",
	"date('5373484')",
	"julianday('2026-01-01T00:00:00.0004')",
	"julianday('2026-01-01T00:00:00.0005')",
	"julianday('2026-01-01T00:00:00.123456789')",
	"julianday('2026-12-31T23:59:59.9999')",
	"julianday(2460000.5000004)",
	"julianday(2729462.7741404455)",
	"julianday(4740199.40014776)",
	"datetime(2460000.99999999)",
	"datetime(2460000.999999999999)",
	"datetime(2460000.9999999999999)",
	"julianday(2460000.99999999)",
	"datetime('2460000.99999999')",
	"date(2460000.99999999)",
	"date('2026-03-31', 5)",
	"date('2026-03-31', 0)",
	"date('2026-03-31', 1.5)",
	"datetime('2026-03-31', -1)",
	"date('2026-03-31', NULL)",
	"date(+0x1f4bd8)",
	"date('nan')",
	"date('0x1p+21')",
	"date('2_460_000.5')",
	"datetime(created_at, '+100000000 days')",
	"datetime(created_at, '-3000000 days')",
	"datetime(created_at, '+1e300 days')",
	"datetime(created_at, '+2000000000 days')",
	"date(5373484.5)",
	"date('5373484.5')",
	"date(5373484.4999)",
	"julianday(created_at, '+0.0004 seconds')",
	"julianday(created_at, '+0.5 seconds')",
	"julianday(created_at, '+1 second', '+0.0004 seconds')",
	"julianday(created_at, '+0.0004 seconds', '+0.0004 seconds')",
	"julianday(created_at, '+0.0004 seconds', '+0.0004 seconds', '+0.0004 seconds')",
	"julianday(created_at, '+0.0006 seconds', '+0.0006 seconds')",
	"julianday(created_at, '+0.0004 seconds', '-0.0004 seconds')",
	"strftime('%Y', created_at, '+0.0004 seconds', '+0.0004 seconds')",
	"datetime(created_at, '+0.0004 seconds', '+0.0004 seconds')",
	"date(-0x10)",
	"datetime(+0x1f4bd8)",
	"datetime(2460000.5000004)",
	"date('not a time')",
	"datetime('14:05:09')",
	"date('2026-09-21T14:05:09+02:00')",
	"strftime('%Q', created_at)",
}

var timeMoments = []string{
	"2026-09-21T14:05:09.123Z",
	"2026-01-31T23:30:00.000Z",
	"2024-02-29T00:00:00.000Z",
	"2026-12-31T23:59:59.999Z",
	"2026-03-01",
	"2026-09-21T14:05:09+02:00",
	"2026-09-21T14:05:09.750-05:30",
	"2026-07-04T00:00:00.001Z",
	"2026-09-21T14:05:09.1235Z",
	"2026-12-31T23:59:59.9999Z",
	"9999-12-31T23:59:59.9999Z",
	"2026-09-21T14:05:09.123456789Z",
	"2026-01-01T00:00:00.0004Z",
	"2026-03-31T05:06:07+14:59",
	"2026-03-31T05:06:07+15:30",
	"2026-03-31T05:06:07+23:59",
	"2026-01-01T00:00:00.1234999Z",
	"2026-01-01T00:00:00.1235000Z",
	"2026-01-01T00:00:00.12349999999Z",
	"2026-01-01T00:00:00.9994999Z",
	"2026-01-01T00:00:00.1234Z",
	"2026-01-01T00:00:00.9995Z",
	"2026-01-01T00:00:00.9994Z",
	"2026-12-31T23:59:59.9996Z",
	"2026-03-19T14:05:09.123Z",
}

func sqliteScalar(t *testing.T, db *sql.DB, expr, moment string) (string, bool) {
	t.Helper()
	var out sql.NullString
	query := "SELECT " + strings.ReplaceAll(expr, "created_at", "?")
	binds := make([]any, strings.Count(query, "?"))
	for i := range binds {
		binds[i] = moment
	}
	if err := db.QueryRow(query, binds...).Scan(&out); err != nil {
		t.Fatalf("oracle %q: %v", expr, err)
	}
	return out.String, out.Valid
}

func sameScalar(got, want string) bool {
	if got == want {
		return true
	}
	a, aerr := strconv.ParseFloat(got, 64)
	b, berr := strconv.ParseFloat(want, 64)
	return aerr == nil && berr == nil && a == b
}

func TestEveryDateExpressionAnswersWhatSQLiteAnswers(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	db := sqliteOracle(t)
	for _, moment := range timeMoments {
		for _, expr := range timeExpressions {
			want, wantValid := sqliteScalar(t, db, expr, moment)
			rendered := renderValueFor(t, expr)
			var got sql.NullString
			query := "SELECT (" + rendered + ")::text FROM (VALUES (" + dollarQuote(moment) + "::text)) AS t(created_at)"
			if err := s.pool.QueryRow(t.Context(), query).Scan(&got); err != nil {
				t.Errorf("%s at %s: %v\n%s", expr, moment, err, query)
				continue
			}
			if got.Valid != wantValid {
				t.Errorf("%s at %s: SQLite %v %q, adapter #2 %v %q", expr, moment, wantValid, want, got.Valid, got.String)
				continue
			}
			if wantValid && !sameScalar(got.String, want) {
				t.Errorf("%s at %s: SQLite says %q, adapter #2 says %q", expr, moment, want, got.String)
			}
		}
	}
}

func renderValueFor(t *testing.T, expr string) string {
	t.Helper()
	node, err := filter.Parse(expr, filter.Options{Columns: []string{"id", "body", "created_at"}})
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	r := &filterRenderer{
		columns: map[string]string{"id": "id", "body": "c1", "created_at": "created_at"},
		types:   map[string]schema.FieldType{"id": schema.Number, "body": schema.Text, "created_at": schema.Timestamp},
	}
	out, err := r.capture(node, 1)
	if err != nil {
		t.Fatalf("render(%q): %v", expr, err)
	}
	return out
}

func renderErrFor(t *testing.T, expr string) error {
	t.Helper()
	node, err := filter.Parse(expr, filter.Options{Columns: []string{"id", "body", "created_at"}})
	if err != nil {
		t.Fatalf("the shared language accepts %q, so this test is about what the engine does with it: %v", expr, err)
	}
	_, _, err = renderScopedFilter(node,
		map[string]string{"id": "id", "body": "c1", "created_at": "created_at"},
		map[string]schema.FieldType{"id": schema.Number, "body": schema.Text, "created_at": schema.Timestamp},
		nil, 1)
	return err
}

func TestWhatThisEngineWillNotRenderIsRefusedRatherThanAnswered(t *testing.T) {
	db := sqliteOracle(t)
	for _, expr := range []string{
		"date(created_at, '+1 month')",
		"date(created_at, '-2 years')",
		"date(created_at, 'start of month')",
		"date(created_at, 'weekday 0')",
		"strftime('%f', created_at)",
		"strftime('%s', created_at)",
		"strftime('%w', created_at)",
		"strftime('%W', created_at)",
		"date(body)",
		"date('2026-09-21 24:00:00')",
		"date('-2026-09-21')",
		"date(0)",
		"date('0')",
		"datetime('1000000')",
		"date('1721425')",
		"datetime(0)",
		"date(1000000)",
		"date('0000-01-01')",
		"date('1e6')",
		"date(+0x10)",
		"julianday(+0x10)",
		"strftime(body, created_at)",
		"julianday(created_at) || ''",
		"date(strftime('%Y-%m-%d', created_at))",
		"julianday(strftime('%j', created_at))",
		"julianday(created_at) LIKE '24%'",
		"created_at > julianday(created_at)",
		"body = julianday(created_at)",
	} {
		err := renderErrFor(t, expr)
		if err == nil {
			t.Errorf("%s: rendered without complaint; if this engine cannot reproduce SQLite's answer it must refuse, because these filters drive delete", expr)
			continue
		}
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: refused with %v, which is not the invalid-request class every other refusal in this renderer carries", expr, err)
		}
		var out sql.NullString
		query := "SELECT " + strings.ReplaceAll(strings.ReplaceAll(expr, "created_at", "'2026-09-21 14:05:09'"), "body", "'2026-03-04'")
		if err := db.QueryRow(query).Scan(&out); err != nil {
			t.Fatalf("oracle %q: %v", query, err)
		}
		if !out.Valid {
			t.Errorf("%s: refused, but SQLite answers NULL for it, so there is nothing here this engine fails to reproduce", expr)
		}
	}
}

func TestANestedStrftimeIsRefusedByName(t *testing.T) {
	err := renderErrFor(t, "date(strftime('%Y-%m-%d', created_at))")
	if err == nil || !strings.Contains(err.Error(), "strftime inside another date or time function") {
		t.Fatalf("a nested strftime must be refused by name, since SQLite reparses its text and reads an all-digit one as a Julian day: %v", err)
	}
}

var mixedClassTimeExpressions = []string{
	"date(created_at) > 1",
	"date(created_at) = 1",
	"date(created_at) <= 1",
	"strftime('%Y', created_at) = 2026",
	"strftime('%Y', created_at) = '2026'",
	"julianday(created_at) > 'abc'",
	"julianday(created_at) > '1'",
	"julianday(created_at) = '2461305'",
	"julianday(created_at) = 2461305",
	"datetime(created_at) != 0",
	"date(created_at) + 1",
	"strftime('%Y', created_at) + 1",
	"strftime('%j', created_at) / 2",
	"abs(date(created_at))",
	"NOT date(created_at)",
	"NOT time(created_at)",
	"date(created_at) AND 1",
	"julianday(created_at) + 1",
	"julianday(created_at) / 2",
	"julianday(created_at) - 2461305",
	"round(julianday(created_at), 3)",
	"length(date(created_at))",
	"julianday(created_at) > 2400000",
	"date(created_at) IN ('2026-09-21', 1)",
	"julianday(created_at) BETWEEN 2461305 AND '1'",
	"time(created_at) IS '12:00:00'",
	"coalesce(date(created_at), 'x') = '2026-09-21'",
	"created_at > date(created_at)",
}

var mixedClassMoments = []string{
	"2026-09-21T14:05:09.123Z",
	"2026-09-21T12:00:00.000Z",
	"2026-09-21T00:06:07.000Z",
}

func TestAMixedClassTimeExpressionAnswersWhatSQLiteAnswers(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	db := sqliteOracle(t)
	for _, moment := range mixedClassMoments {
		for _, expr := range mixedClassTimeExpressions {
			want, wantValid := sqliteScalar(t, db, expr, moment)
			rendered := renderValueFor(t, expr)
			var got sql.NullString
			query := "SELECT (" + rendered + ")::text FROM (VALUES (" + dollarQuote(moment) + "::text)) AS t(created_at)"
			if err := s.pool.QueryRow(t.Context(), query).Scan(&got); err != nil {
				t.Errorf("%s at %s: %v\n%s", expr, moment, err, query)
				continue
			}
			if got.Valid != wantValid || (wantValid && !answersAlike(got.String, want)) {
				t.Errorf("%s at %s: SQLite answers %v %q and this engine answers %v %q", expr, moment, wantValid, want, got.Valid, got.String)
			}
		}
	}
}

func TestATimeFunctionComparedAsTextCarriesTheBinaryCollation(t *testing.T) {
	for _, tc := range []struct {
		expr string
		args []any
	}{
		{"date(created_at) = '2026-09-21'", nil},
		{"time(created_at) < ?", []any{"12:00:00"}},
		{"strftime('%Y', created_at) > created_at", nil},
		{"datetime(created_at) IN ('a', 'b')", nil},
	} {
		expr := tc.expr
		sql := renderFor(t, expr, tc.args...)
		if !strings.Contains(sql, `COLLATE "C"`) {
			t.Errorf("%s rendered without the binary collation, so it answers by whatever collation the database was created with: %s", expr, sql)
		}
	}
}

func answersAlike(got, want string) bool {
	switch want {
	case "1":
		if got == "true" {
			return true
		}
	case "0":
		if got == "false" {
			return true
		}
	}
	return sameScalar(got, want)
}

func TestAStoredMomentAgreesUnlessItPredatesTheCommonEra(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	db := sqliteOracle(t)
	answered := func(stored string) (sql.NullString, sql.NullString) {
		t.Helper()
		var want sql.NullString
		if err := db.QueryRow("SELECT date(?)", stored).Scan(&want); err != nil {
			t.Fatalf("oracle date(%q): %v", stored, err)
		}
		var got sql.NullString
		query := "SELECT (" + renderValueFor(t, "date(created_at)") + ")::text FROM (VALUES (" +
			dollarQuote(stored) + "::text)) AS t(created_at)"
		if err := s.pool.QueryRow(t.Context(), query).Scan(&got); err != nil {
			t.Fatalf("%q: %v", stored, err)
		}
		return want, got
	}

	for _, stored := range []string{"0001-01-01", "0001-01-01T00:00:00Z", "9999-12-31T23:59:59.999Z", "1582-10-15"} {
		want, got := answered(stored)
		if !got.Valid || got.String != want.String {
			t.Errorf("a stored %q reads as %q on SQLite and %q here; only a year before 0001 is allowed to diverge", stored, want.String, got.String)
		}
	}

	for _, stored := range []string{"0000-01-01", "0000-12-31T23:59:59"} {
		want, got := answered(stored)
		if !want.Valid {
			t.Fatalf("SQLite answers nothing for a stored %q, so there is no divergence left to record", stored)
		}
		if got.Valid {
			t.Fatalf("a stored %q now reads as %q rather than NULL: the era gap has closed on the column path, so delete this test and the paragraph in docs/design/postgresql.md that records it", stored, got.String)
		}
	}
}

func TestAShiftOutOfTheCommonEraAnswersNothingRatherThanTheWrongYear(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	db := sqliteOracle(t)
	for _, expr := range []string{
		"date('0001-01-01', '-1 day')",
		"datetime('0001-01-01 00:00:00', '-1 second')",
		"julianday('0001-01-01', '-1 day')",
	} {
		want, wantValid := sqliteScalar(t, db, expr, "2026-09-21T14:05:09.123Z")
		if !wantValid {
			t.Fatalf("SQLite answers nothing for %s, so there is no divergence left to record", expr)
		}
		var got sql.NullString
		query := "SELECT (" + renderValueFor(t, expr) + ")::text"
		if err := s.pool.QueryRow(t.Context(), query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got.Valid {
			if got.String == want {
				t.Fatalf("%s now answers %q as SQLite does: the era gap has closed, so fold these cases back into timeExpressions and delete the paragraph in docs/design/postgresql.md that records it", expr, got.String)
			}
			t.Fatalf("%s answers %q where SQLite says %q. PostgreSQL writes that era BC and to_char drops the marker, so the only safe answer is nothing at all", expr, got.String, want)
		}
	}
}
