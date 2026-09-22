package postgres

import (
	"database/sql"
	"errors"
	"math"
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
	"datetime('2460000.5')",
	"datetime(2460000.5)",
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
	return aerr == nil && berr == nil && math.Abs(a-b) < 1e-8
}

func TestEveryDateExpressionAnswersWhatSQLiteAnswers(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	db := sqliteOracle(t)
	for _, moment := range timeMoments {
		for _, expr := range timeExpressions {
			want, wantValid := sqliteScalar(t, db, expr, moment)
			rendered := renderFor(t, expr)
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
		"strftime(body, created_at)",
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

var mixedClassTimeExpressions = []string{
	"date(created_at) > 1",
	"date(created_at) = 1",
	"date(created_at) <= 1",
	"strftime('%Y', created_at) = 2026",
	"julianday(created_at) > 'abc'",
	"datetime(created_at) != 0",
	"date(created_at) + 1",
	"strftime('%Y', created_at) + 1",
	"abs(date(created_at))",
	"NOT date(created_at)",
	"date(created_at) AND 1",
	"julianday(created_at) + 1",
	"length(date(created_at))",
	"julianday(created_at) > 2400000",
}

func TestAMixedClassTimeExpressionRaisesRatherThanAnsweringDifferently(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	db := sqliteOracle(t)
	const moment = "2026-09-21T14:05:09.123Z"
	raised := 0
	for _, expr := range mixedClassTimeExpressions {
		want, wantValid := sqliteScalar(t, db, expr, moment)
		rendered := renderFor(t, expr)
		var got sql.NullString
		query := "SELECT (" + rendered + ")::text FROM (VALUES (" + dollarQuote(moment) + "::text)) AS t(created_at)"
		if err := s.pool.QueryRow(t.Context(), query).Scan(&got); err != nil {
			raised++
			continue
		}
		if got.Valid != wantValid || (wantValid && !answersAlike(got.String, want)) {
			t.Errorf("%s: SQLite answers %q and this engine answers %q. Disagreeing is allowed here only by raising: an expression that quietly returns the other row set is a wrong delete", expr, want, got.String)
		}
	}
	if raised == 0 {
		t.Fatal("not one of these expressions raised, so the storage-class gap has closed: rewrite this test to assert agreement, and delete the paragraph in docs/design/postgresql.md that describes the divergence, in this same change")
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
