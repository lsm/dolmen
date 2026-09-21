package filter

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func sqliteWithTable(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE notes (
		id INTEGER PRIMARY KEY,
		created_at TEXT,
		owner TEXT,
		title TEXT,
		body TEXT,
		score REAL,
		secret INTEGER,
		due TEXT
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO notes (created_at, owner, title, body, score, secret, due)
		VALUES ('2026-01-01T00:00:00Z', 'alice', 'a title', 'a body', 1.5, 7, '2026-06-01')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

func TestEveryAcceptedExpressionIsValidSQLite(t *testing.T) {
	db := sqliteWithTable(t)
	for _, expr := range acceptedExpressions {
		args := make([]any, strings.Count(expr, "?"))
		for i := range args {
			args[i] = "a-bound-value"
		}
		rows, err := db.Query(`SELECT id FROM notes WHERE `+expr, args...)
		if err != nil {
			t.Fatalf("the validator accepts %q but sqlite refuses it, so a caller would meet an engine error instead of a clear one: %v", expr, err)
		}
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatalf("the validator accepts %q but sqlite fails while evaluating it: %v", expr, err)
		}
	}
}

func TestPinnedSQLiteSemantics(t *testing.T) {
	db := sqliteWithTable(t)
	for _, tc := range []struct {
		what string
		expr string
		want any
	}{
		{"like is ascii case-insensitive", `'A' LIKE 'a'`, int64(1)},
		{"string comparison is byte-wise", `'A' < 'a'`, int64(1)},
		{"integer division truncates toward zero", `-7 / 2`, int64(-3)},
		{"round goes half away from zero", `round(2.5)`, 3.0},
		{"round goes half away from zero for negatives", `round(-2.5)`, -3.0},
		{"null propagates through comparison", `(NULL = 1) IS NULL`, int64(1)},
		{"null is not distinct under is", `(NULL IS NULL)`, int64(1)},
		{"text coerces to number in arithmetic", `'3' + 1`, int64(4)},
		{"concatenation coerces numbers to text", `1 || '2'`, "12"},
	} {
		var got any
		if err := db.QueryRow(`SELECT ` + tc.expr).Scan(&got); err != nil {
			t.Fatalf("%s: %q failed: %v", tc.what, tc.expr, err)
		}
		if got != tc.want {
			t.Fatalf("%s: %q gave %#v, want %#v — an engine that differs here must implement this, not its own", tc.what, tc.expr, got, tc.want)
		}
	}
}
