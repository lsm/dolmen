package postgres

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func sqliteOracle(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open the oracle: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sqliteDatetime(t *testing.T, db *sql.DB, expr string, args ...any) (string, bool) {
	t.Helper()
	var out sql.NullString
	if err := db.QueryRow("SELECT datetime("+expr+")", args...).Scan(&out); err != nil {
		t.Fatalf("oracle datetime(%s): %v", expr, err)
	}
	return out.String, out.Valid
}

var timeValueCorpus = []string{
	"2026-09-21",
	"2026-09-21 14:05:09",
	"2026-09-21T14:05:09",
	"2026-09-21T14:05",
	"2026-09-21 14:05",
	"2026-09-21T14:05:09.123",
	"2026-09-21T14:05:09.1",
	"2026-09-21T14:05:09.123456",
	"2026-09-21T14:05:09.123Z",
	"2026-09-21T14:05:09.123z",
	"2026-09-21T14:05:09+02:00",
	"2026-09-21T14:05:09-05:30",
	"2026-09-21 14:05:09Z",
	"2026-09-21T14:05:09.123 ",
	"2026-09-21 ",
	" 2026-09-21",
	"2026-09-21t14:05:09",
	"2026-9-21",
	"26-09-21",
	"2026-13-01",
	"2026-00-15",
	"2026-09-00",
	"2026-02-30",
	"2026-09-21 25:00:00",
	"2026-09-21 14:75:00",
	"0000-01-01",
	"14:05:09",
	"14:05",
	"14:05:09.987",
	"14:05+02:00",
	"2460000",
	"2460000.5",
	"2460000.",
	"  2460000.5  ",
	"-2460000",
	"not a time",
	"",
	"2026",
	"99999999",
	"2026-09-21 24:00:00",
	"2026-09-21T00:00:00.000Z",
	"9999-12-31 23:59:59",
	"2026-09-21+02:00",
	"2026-02-32",
	"2026-01-32",
	"2026-09-99",
	"2026-09-32 14:05:09",
	"2026-02-31",
	"2026-09-21  14:05:09",
	"2026-09-21   14:05:09",
	"2026-09-21\t14:05:09",
	"2026-09-21\n14:05:09",
	"2026-09-21 T14:05:09",
	"2026-09-21T 14:05:09",
	"2026-09-21 T 14:05:09",
	"2026-09-21TT14:05:09",
	"2026-09-2114:05:09",
	"-2026-09-21",
	"-0001-09-21",
	"-2026-09-21 14:05:09",
	"-2026-09-32",
	"+2026-09-21",
	"2026-09-21\t",
	"2026-09-21  14:05:09+02:00",
	"2026-01-01T00:00:00.0004",
	"2026-01-01T00:00:00.0005",
	"2026-01-01T00:00:00.0015",
	"2026-12-31T23:59:59.9995",
	"2026-12-31T23:59:59.9999",
	"2026-01-01T00:00:00.123456789",
	"0000-01-01",
	"0000-12-31T23:59:59",
	"0001-01-01",
	"2026-03-31T05:06:07+14:59",
	"2026-03-31T05:06:07+15:00",
	"2026-03-31T05:06:07+15:59",
	"2026-03-31T05:06:07+16:00",
	"2026-03-31T05:06:07+23:59",
	"2026-03-31T05:06:07-14:59",
	"2026-03-31T05:06:07-15:00",
	"2026-03-31T05:06:07-23:59",
	"2026-03-31T05:06:07+14:60",
	"05:06:07+15:00",
}

func TestTheTimeParserAgreesWithSQLiteOnEveryShape(t *testing.T) {
	db := sqliteOracle(t)
	for _, text := range timeValueCorpus {
		want, wantValid := sqliteDatetime(t, db, "?", text)
		moment, kind := parseSQLiteTimeText(text)
		switch kind {
		case timeReadable:
			if !wantValid {
				t.Errorf("%q: the parser reads a time out of it, but SQLite answers NULL", text)
				continue
			}
			if formatted := moment.Format("2006-01-02 15:04:05"); formatted != want {
				t.Errorf("%q: SQLite reads it as %q, the parser reads it as %q", text, want, formatted)
			}
		case timeMalformed:
			if wantValid {
				t.Errorf("%q: the parser calls it malformed, but SQLite reads it as %q — a refusal is safe, answering NULL is not", text, want)
			}
		default:
			if !wantValid {
				t.Errorf("%q: the parser refuses it as something this engine will not reproduce, but SQLite answers NULL, so there is nothing to refuse", text)
			}
		}
	}
}

var modifierCorpus = []string{
	"+1 day", "-1 day", "+1 days", "+1 DaY", "1 day", "+0 day",
	"+1 hour", "+1 hours", "+90 minute", "+90 minutes", "-1 second", "+1 seconds",
	"+1.5 days", "+.5 hours", "+1e2 seconds", "+0.5 seconds",
	"+1 month", "+1 months", "-2 years", "+1 year",
	"start of day", "start of month", "start of year",
	"weekday 0", "weekday 3", "auto", "subsec", "subsecond", "ceiling", "floor",
	"+01:30", "-01:30", "+00:00:30",
	"+1 week", "+1 weeks", "+ 1 day", "+1day", "days", "+1 dayz", "+1 hours ", " +1 hours",
	"bogus", "", "+1  day", "+1\tday",
}

func TestTheModifierGrammarAgreesWithSQLiteOnWhatItAccepts(t *testing.T) {
	db := sqliteOracle(t)
	const base = "2026-09-21 12:00:00"
	start, err := time.Parse("2006-01-02 15:04:05", base)
	if err != nil {
		t.Fatal(err)
	}
	for _, mod := range modifierCorpus {
		want, wantValid := sqliteDatetime(t, db, "?, ?", base, mod)
		seconds, kind := parseSQLiteModifier(mod)
		switch kind {
		case malformedInSQLite:
			if wantValid {
				t.Errorf("%q: the grammar calls it malformed, but SQLite answers %q", mod, want)
			}
		case sqliteOnly:
			if !wantValid {
				t.Errorf("%q: the grammar calls it a modifier this engine cannot render, but SQLite rejects it outright", mod)
			}
		case supportedHere:
			if !wantValid {
				t.Errorf("%q: the grammar shifts by %v seconds, but SQLite rejects it", mod, seconds)
				continue
			}
			got := start.Add(time.Duration(seconds * float64(time.Second))).Format("2006-01-02 15:04:05")
			if got != want {
				t.Errorf("%q: SQLite shifts to %q, the grammar shifts to %q", mod, want, got)
			}
		}
	}
}

func TestAnHourOfTwentyFourIsRefusedRatherThanNormalised(t *testing.T) {
	db := sqliteOracle(t)
	const text = "2026-09-21 24:00:00"
	if _, kind := parseSQLiteTimeText(text); kind != timeUnrendered {
		t.Fatalf("%q must be refused: %v", text, kind)
	}
	want, valid := sqliteDatetime(t, db, "?", text)
	if !valid || want != text {
		t.Fatalf("SQLite carries the hour 24 through formatting unchanged (%q, %v), which is the state this engine will not reproduce", want, valid)
	}
}

func TestTheClockWordsAreRefusedRatherThanRead(t *testing.T) {
	db := sqliteOracle(t)
	for _, word := range []string{"now", "NOW", " now ", "localtime", "utc"} {
		if _, kind := parseSQLiteTimeText(word); kind != timeClockDependent {
			t.Errorf("%q must be refused as a clock read, not parsed or called malformed: %v", word, kind)
		}
	}
	if _, valid := sqliteDatetime(t, db, "?", "now"); !valid {
		t.Fatal("SQLite reads the clock for 'now', which is why the parser must refuse it rather than answer NULL")
	}
}

func TestTheNumericOnlyModifiersAreAcceptedOverANumericTime(t *testing.T) {
	db := sqliteOracle(t)
	for _, mod := range []string{"unixepoch", "julianday"} {
		if _, kind := parseSQLiteModifier(mod); kind != sqliteOnly {
			t.Errorf("%q must be refused rather than answered: %v", mod, kind)
		}
	}
	if _, valid := sqliteDatetime(t, db, "?, ?", "1789999509", "unixepoch"); !valid {
		t.Fatal("unixepoch is a modifier SQLite accepts over a numeric time value, so answering NULL for it would be a wrong answer rather than a refusal")
	}
}

var strftimeCorpus = []struct {
	format string
	kind   filterSupport
}{
	{"%Y", supportedHere},
	{"%Y-%m-%d", supportedHere},
	{"%Y-%m-%dT%H:%M:%S", supportedHere},
	{"%d/%m/%Y", supportedHere},
	{"literal %Y text", supportedHere},
	{"100%%", supportedHere},
	{"%j", supportedHere},
	{`a "quoted" %Y`, supportedHere},
	{"%H:%M", supportedHere},
	{"%f", sqliteOnly},
	{"%s", sqliteOnly},
	{"%w", sqliteOnly},
	{"%W", sqliteOnly},
	{`x\`, supportedHere},
	{`a\b`, supportedHere},
	{`\`, supportedHere},
	{`\\`, supportedHere},
	{`%Y\`, supportedHere},
	{`a"b`, supportedHere},
	{`a\"b`, supportedHere},
	{`\%Y`, supportedHere},
	{`%Y"\`, supportedHere},
	{"%Q", malformedInSQLite},
	{"%", malformedInSQLite},
}

func TestTheRenderedFormatSaysWhatSQLitesStrftimeSays(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	db := sqliteOracle(t)
	const moment = "2026-09-21 06:07:08"
	for _, tc := range strftimeCorpus {
		pattern, unsupported, kind := strftimeToCharPattern(tc.format)
		if kind != tc.kind {
			t.Errorf("%q: classified %v (code %q), want %v", tc.format, kind, unsupported, tc.kind)
			continue
		}
		var want sql.NullString
		if err := db.QueryRow("SELECT strftime(?, ?)", tc.format, moment).Scan(&want); err != nil {
			t.Fatalf("oracle strftime(%q): %v", tc.format, err)
		}
		if kind != supportedHere {
			if want.Valid != (kind == sqliteOnly) {
				t.Errorf("%q: SQLite answers %v, which does not match refusing it as %v", tc.format, want, kind)
			}
			continue
		}
		if !want.Valid {
			t.Errorf("%q: the renderer translates it, but SQLite answers NULL", tc.format)
			continue
		}
		var got string
		if err := s.pool.QueryRow(t.Context(), "SELECT pg_catalog.to_char($1::timestamp, $2)", moment, pattern).Scan(&got); err != nil {
			t.Fatalf("%q rendered as %q: %v", tc.format, pattern, err)
		}
		if got != want.String {
			t.Errorf("%q rendered as %q: PostgreSQL says %q, SQLite says %q", tc.format, pattern, got, want.String)
		}
	}
}
