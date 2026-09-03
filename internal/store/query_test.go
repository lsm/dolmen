package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestCreateInsertQuery(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	ids := mustInsertNotes(t, st)
	if len(ids) != 3 {
		t.Fatalf("expected 3 ids, got %d", len(ids))
	}

	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil, 0, 0)
	if err != nil {
		t.Fatalf("query count: %v", err)
	}
	if got := rows[0]["n"].(int64); got != 3 {
		t.Fatalf("expected 3 rows, got %d", got)
	}

	rows, _, err = st.Query(ctx, "test", "SELECT title FROM notes WHERE score > ? ORDER BY score DESC", []any{2}, 0, 0)
	if err != nil {
		t.Fatalf("query filter: %v", err)
	}
	if len(rows) != 2 || rows[0]["title"] != "first note" || rows[1]["title"] != "second note" {
		t.Fatalf("unexpected rows: %v", rows)
	}

	if _, _, err := st.Query(ctx, "test", "DELETE FROM notes", nil, 0, 0); err == nil {
		t.Fatal("expected DELETE via query to be rejected")
	}
	if _, _, err := st.Query(ctx, "test", "INSERT INTO notes(title) VALUES('x')", nil, 0, 0); err == nil {
		t.Fatal("expected INSERT via query to be rejected")
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1; DROP TABLE notes", nil, 0, 0); err == nil {
		t.Fatal("expected multi-statement to be rejected")
	}

	sc, count, err := st.DescribeTable(ctx, "test", "notes")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if sc.Version != 1 || count != 3 || len(sc.Fields) != 6 {
		t.Fatalf("unexpected describe: v%d rows=%d fields=%d", sc.Version, count, len(sc.Fields))
	}
}

func TestHasStatementSeparator(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"SELECT 1", false},
		{"SELECT 'a;b'", false},
		{"SELECT 'a'';b'", false},
		{`SELECT "a;b" FROM t`, false},
		{"SELECT `a;b` FROM t", false},
		{"SELECT [a;b] FROM t", false},
		{"SELECT 1 -- ; comment", false},
		{"SELECT 1 -- don't; parse\nFROM t", false},
		{"SELECT /* ; comment */ 1", false},
		{"SELECT 1; SELECT 2", true},
		{"SELECT 'a'; DROP TABLE t", true},
		{`SELECT "a" FROM t; SELECT 2`, true},
		{"SELECT `a` FROM t; SELECT 2", true},
		{"SELECT [a] FROM t; SELECT 2", true},
		{"SELECT 1 -- fine\n; SELECT 2", true},
		{"SELECT /* ; */ 1; SELECT 2", true},
	}
	for _, c := range cases {
		if got := hasStatementSeparator(c.sql); got != c.want {
			t.Errorf("hasStatementSeparator(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
}

func TestQuerySemicolonInsideQuotesAllowed(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	allowed := []string{
		"SELECT 'a;b' AS v",
		"SELECT title FROM notes WHERE title = 'a;b'",
		"SELECT title FROM notes WHERE title <> 'x'';y'",
		`SELECT count(*) AS n FROM "notes"`,
		"SELECT count(*) AS n FROM `notes`",
		"SELECT count(*) AS n FROM [notes]",
		"SELECT count(*) AS n FROM notes -- ; not a break",
		"SELECT /* ; not a break */ count(*) AS n FROM notes",
	}
	for _, q := range allowed {
		if _, _, err := st.Query(ctx, "test", q, nil, 0, 0); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}

	rejected := []string{
		"SELECT 1; SELECT 2",
		"SELECT 'a'; DROP TABLE notes",
		`SELECT "title" FROM notes; SELECT 2`,
		"SELECT `title` FROM notes; SELECT 2",
		"SELECT [title] FROM notes; SELECT 2",
		"SELECT 1 -- fine\n; SELECT 2",
		"SELECT /* ; */ 1; SELECT 2",
	}
	for _, q := range rejected {
		if _, _, err := st.Query(ctx, "test", q, nil, 0, 0); err == nil {
			t.Fatalf("expected multi-statement query %q to be rejected", q)
		}
	}
}

func TestQuerySemicolonLiteralRoundTrip(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "a;b", "body": "semicolon title"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT title FROM notes WHERE title = 'a;b'", nil, 0, 0)
	if err != nil {
		t.Fatalf("query with semicolon literal: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "a;b" {
		t.Fatalf("unexpected rows: %v", rows)
	}
}

func TestInsertEmptyRecordDefaultValues(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "opts", []schema.Field{
		{Name: "x", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	ids, err := st.Insert(ctx, "test", "opts", []map[string]any{{}}, testEmbed)
	if err != nil {
		t.Fatalf("insert empty record: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 id, got %v", ids)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM opts", nil, 0, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got := rows[0]["n"].(int64); got != 1 {
		t.Fatalf("expected 1 row, got %d", got)
	}
}

func TestInferCreateInsertRoundTrip(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	samples := []map[string]any{
		{"flag": true, "note": "unknown"},
		{"flag": "maybe", "note": "2"},
	}
	fields := schema.InferFields(samples)
	if _, err := st.CreateTable(ctx, "test", "mixed", fields); err != nil {
		t.Fatalf("create from inferred schema: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "mixed", samples, testEmbed); err != nil {
		t.Fatalf("inserting the same samples must work: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT flag, note FROM mixed ORDER BY id", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 || rows[0]["flag"] != true || rows[0]["note"] != "unknown" {
		t.Fatalf("unexpected rows: %v", rows)
	}
	if rows[1]["flag"] != "maybe" {
		t.Fatalf("json field must decode to its JSON value: %v", rows)
	}
}

func TestInferCreateAcceptsSanitizedNames(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	samples := []map[string]any{
		{"1st": "one", "my-field": "two", "ID": "three", "created_at": "four", "Name": "Alice", "name": "Bob"},
	}
	fields := schema.InferFields(samples)
	if _, err := st.CreateTable(ctx, "test", "sanitized", fields); err != nil {
		t.Fatalf("create from sanitized inferred schema: %v", err)
	}
}

func TestJSONFieldStringScalarsAreValidJSON(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "jf", []schema.Field{
		{Name: "v", Type: schema.JSON},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "jf", []map[string]any{
		{"v": "unknown"},
		{"v": map[string]any{"state": "ok"}},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT json_extract(v, '$') AS decoded FROM jf ORDER BY id", nil, 0, 0)
	if err != nil {
		t.Fatalf("json_extract over json field: %v", err)
	}
	if rows[0]["decoded"] != "unknown" || rows[1]["decoded"] != `{"state":"ok"}` {
		t.Fatalf("json_extract results wrong: %v", rows)
	}
}

func TestZeroVectorTypedInQuery(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "zero", "emb": []any{0, 0, 0, 0}},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT emb FROM notes WHERE title = 'zero'", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	vec, ok := rows[0]["emb"].([]float64)
	if !ok {
		t.Fatalf("expected a typed vector, got %T", rows[0]["emb"])
	}
	if len(vec) != 4 || vec[0] != 0 || vec[3] != 0 {
		t.Fatalf("expected [0 0 0 0], got %v", vec)
	}
}

func TestIntegerPrecisionPreserved(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "prec", []schema.Field{
		{Name: "n", Type: schema.Number},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "prec", []map[string]any{
		{"n": json.Number("9007199254740993")},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT n FROM prec", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["n"].(int64) != 9007199254740993 {
		t.Fatalf("integer precision lost: %v", rows[0]["n"])
	}
}

func TestQueryResultCap(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "cap", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for b := 0; b < 2; b++ {
		records := make([]map[string]any, 0, 600)
		for i := 0; i < 600; i++ {
			records = append(records, map[string]any{"v": "x"})
		}
		if _, err := st.Insert(ctx, "test", "cap", records, testEmbed); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	rows, truncated, err := st.Query(ctx, "test", "SELECT * FROM cap", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1000 || !truncated {
		t.Fatalf("expected capped 1000 rows with truncated=true, got %d truncated=%v", len(rows), truncated)
	}
	rows, truncated, err = st.Query(ctx, "test", "SELECT count(*) AS n FROM cap", nil, 0, 0)
	if err != nil || truncated {
		t.Fatalf("small query must not be truncated: %v %v", err, truncated)
	}
	if rows[0]["n"].(int64) != 1200 {
		t.Fatalf("expected 1200 total, got %v", rows[0]["n"])
	}
}

func TestTruncationFlagAccuracy(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "exact", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	records := make([]map[string]any, 0, 1000)
	for i := 0; i < 1000; i++ {
		records = append(records, map[string]any{"v": "x"})
	}
	if _, err := st.Insert(ctx, "test", "exact", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, truncated, err := st.Query(ctx, "test", "SELECT * FROM exact", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1000 || truncated {
		t.Fatalf("exactly 1000 rows must not be marked truncated: %d %v", len(rows), truncated)
	}
}

func TestQueryByteBudget(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "bigvals", []schema.Field{
		{Name: "v", Type: schema.Text},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	chunk := strings.Repeat("y", 12<<20)
	for i := 0; i < 4; i++ {
		if _, err := st.Insert(ctx, "test", "bigvals", []map[string]any{{"v": chunk}}, testEmbed); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, truncated, err := st.Query(ctx, "test", "SELECT v FROM bigvals", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("byte budget should cap at 2 of 4 12MiB rows (3rd would exceed 32MiB): %d truncated=%v", len(rows), truncated)
	}
}

func TestOversizedFirstQueryRowRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "any", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT zeroblob(34000000) AS b", nil, 0, 0); err == nil {
		t.Fatal("expected oversized first row to be rejected")
	}
}

func TestNonFiniteQueryValueRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "nf", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1e999 AS x", nil, 0, 0); err == nil {
		t.Fatal("expected non-finite query value to be rejected")
	}
}

func TestDuplicateColumnLabelsRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "dup", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1 AS a, 2 AS a", nil, 0, 0); err == nil {
		t.Fatal("expected duplicate column labels to be rejected")
	}
	// A statement with its own LIMIT takes the wrapped-subquery fallback,
	// where SQLite would rename the duplicate (a, a:1) instead of letting
	// rowsToMaps reject it; the fallback must preserve the rejection.
	if _, _, err := st.Query(ctx, "test", "SELECT 1 AS a, 2 AS a LIMIT 1", nil, 0, 0); err == nil {
		t.Fatal("expected duplicate column labels to be rejected through the wrapped fallback")
	}
}

func TestOversizedColumnLabelRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "lbl", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	longAlias := strings.Repeat("x", 5000)
	if _, _, err := st.Query(ctx, "test", "SELECT 1 AS \""+longAlias+"\"", nil, 0, 0); err == nil {
		t.Fatal("expected oversized column label to be rejected")
	}
}

func TestMalformedQueryIsInvalidRequest(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "mq", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT (", nil, 0, 0); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed SQL to classify as invalid request, got %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1 WHERE 1=?", nil, 0, 0); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected wrong arg count to classify as invalid request, got %v", err)
	}
}

func TestEscapeHeavyStringsBudgeted(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "esc", []schema.Field{
		{Name: "v", Type: schema.Text},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	control := strings.Repeat("\x01", 7<<20)
	if _, err := st.Insert(ctx, "test", "esc", []map[string]any{{"v": control}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT v FROM esc", nil, 0, 0); err == nil {
		t.Fatal("expected escape-heavy row to exceed the response budget")
	}
}

func TestEncodedSizeCoversMandatoryEscapes(t *testing.T) {
	if got := encodedSize(" "); got < 6 {
		t.Fatalf("U+2028 should be charged at encoded size, got %d", got)
	}
	if got := encodedSize("\x80"); got < 6 {
		t.Fatalf("invalid UTF-8 should be charged at the \\ufffd escape size, got %d", got)
	}
	if got := encodedSize("plain"); got != 5 {
		t.Fatalf("plain ascii miscounted: %d", got)
	}
}

func TestQueryStepErrorsAreInvalidRequests(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "step", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT json_extract('x', '$') AS v", nil, 0, 0); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected step-time SQL error to classify as invalid request, got %v", err)
	}
}

func TestLabelBytesEscapeAware(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "lbl2", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	records := make([]map[string]any, 0, 60)
	for i := 0; i < 60; i++ {
		records = append(records, map[string]any{"v": strings.Repeat("\x01", 600000)})
	}
	if _, err := st.Insert(ctx, "test", "lbl2", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	alias := strings.Repeat("\x01", 4000)
	rows, truncated, err := st.Query(ctx, "test", "SELECT v AS \""+alias+"\" FROM lbl2", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) >= 60 || !truncated {
		t.Fatalf("escape-heavy labels must count against the response budget: %d truncated=%v", len(rows), truncated)
	}
}

func TestCumulativeBudgetBeforeNormalization(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "cum", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT zeroblob(30000000) AS a, zeroblob(30000000) AS b", nil, 0, 0); err == nil {
		t.Fatal("expected cumulative oversized row to be rejected before normalization")
	}
}

func TestJSONFieldAcceptsJSONNumbers(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "jn", []schema.Field{
		{Name: "payload", Type: schema.JSON},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "jn", []map[string]any{
		{"payload": json.Number("123")},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT json_extract(payload, '$') AS v FROM jn", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["v"].(int64) != 123 {
		t.Fatalf("numeric json scalar lost: %v", rows[0]["v"])
	}
}

func TestJSONStringScalarsKeepType(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "js", []schema.Field{
		{Name: "v", Type: schema.JSON},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "js", []map[string]any{
		{"v": "true"}, {"v": "123"}, {"v": "null"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT json_extract(v, '$') AS val, typeof(json_extract(v, '$')) AS kind FROM js ORDER BY id", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for i, want := range []string{"true", "123", "null"} {
		if rows[i]["val"] != want || rows[i]["kind"] != "text" {
			t.Fatalf("row %d: string scalar changed type: %v", i, rows[i])
		}
	}
}

func TestQueryPaginationAndTruncatedFlag(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "page", []schema.Field{
		{Name: "v", Type: schema.Number},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	records := make([]map[string]any, 0, 10)
	for i := 0; i < 10; i++ {
		records = append(records, map[string]any{"v": i})
	}
	if _, err := st.Insert(ctx, "test", "page", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Page 0: 3 rows, ordered by id.
	rows, truncated, err := st.Query(ctx, "test", "SELECT v FROM page ORDER BY id", nil, 0, 3)
	if err != nil {
		t.Fatalf("page 0: %v", err)
	}
	if len(rows) != 3 || rows[0]["v"].(int64) != 0 || rows[2]["v"].(int64) != 2 || !truncated {
		t.Fatalf("page 0 should return 3 rows with truncated=true: %v %v", len(rows), truncated)
	}

	// Page 1: next 3 rows.
	rows, truncated, err = st.Query(ctx, "test", "SELECT v FROM page ORDER BY id", nil, 3, 3)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(rows) != 3 || rows[0]["v"].(int64) != 3 || rows[2]["v"].(int64) != 5 || !truncated {
		t.Fatalf("page 1 should return rows 3-5 with truncated=true: %v %v", len(rows), truncated)
	}

	// Page 3: last 1 row, truncated should be false.
	rows, truncated, err = st.Query(ctx, "test", "SELECT v FROM page ORDER BY id", nil, 9, 3)
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(rows) != 1 || rows[0]["v"].(int64) != 9 || truncated {
		t.Fatalf("page 3 should return 1 row with truncated=false: %v %v", len(rows), truncated)
	}

	// Empty page past the end.
	rows, truncated, err = st.Query(ctx, "test", "SELECT v FROM page ORDER BY id", nil, 100, 3)
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if len(rows) != 0 || truncated {
		t.Fatalf("empty page should return 0 rows with truncated=false: %v %v", len(rows), truncated)
	}
}

func TestQueryExactLimitNotTruncated(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "exact5", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	records := make([]map[string]any, 0, 5)
	for i := 0; i < 5; i++ {
		records = append(records, map[string]any{"v": "x"})
	}
	if _, err := st.Insert(ctx, "test", "exact5", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, truncated, err := st.Query(ctx, "test", "SELECT * FROM exact5 ORDER BY id", nil, 0, 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 5 || truncated {
		t.Fatalf("exactly 5 rows must not be truncated: %d %v", len(rows), truncated)
	}
}

func TestQueryTrailingLineComment(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "comm", []schema.Field{
		{Name: "v", Type: schema.Number},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	records := make([]map[string]any, 0, 3)
	for i := 0; i < 3; i++ {
		records = append(records, map[string]any{"v": i})
	}
	if _, err := st.Insert(ctx, "test", "comm", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, truncated, err := st.Query(ctx, "test", "SELECT v FROM comm ORDER BY id -- stable order", nil, 0, 2)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 || !truncated || rows[0]["v"].(int64) != 0 || rows[1]["v"].(int64) != 1 {
		t.Fatalf("trailing line comment must not break pagination: %v %v", rows, truncated)
	}
}

func TestQueryValuesStatement(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	rows, truncated, err := st.Query(ctx, "test", "WITH x(v) AS (VALUES (1),(2),(3)) VALUES (1),(2),(3)", nil, 0, 2)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("page 0 should return 2 rows with truncated=true: %d %v", len(rows), truncated)
	}
	if rows[0]["column1"].(int64) != 1 || rows[1]["column1"].(int64) != 2 {
		t.Fatalf("values rows out of order: %v", rows)
	}

	rows, truncated, err = st.Query(ctx, "test", "WITH x(v) AS (VALUES (1),(2),(3)) VALUES (1),(2),(3)", nil, 2, 2)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || truncated {
		t.Fatalf("page 1 should return 1 row with truncated=false: %d %v", len(rows), truncated)
	}
}

func TestQueryUnterminatedBlockComment(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	rows, truncated, err := st.Query(ctx, "test", "SELECT 1 AS n /* unterminated block comment", nil, 0, 1)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || truncated || rows[0]["n"].(int64) != 1 {
		t.Fatalf("unterminated block comment must not break pagination: %v %v", rows, truncated)
	}
}

func TestQueryBlockCommentInsideQuotedIdentifier(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	rows, _, err := st.Query(ctx, "test", "SELECT 42 AS \"/*literal\"", nil, 0, 1)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0]["/*literal"].(int64) != 42 {
		t.Fatalf("/* inside a quoted identifier must not be treated as a comment: %v", rows)
	}
}

func TestQueryErrorsAreSanitizedAndSelfCorrectable(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	cases := []struct {
		sql       string
		args      []any
		wantErr   string
		wantToken string
	}{
		{"SELECT (", nil, "incomplete SQL statement", ""},
		{"SELECT * FROOOM notes", nil, "SQL syntax error near", "FROOOM"},
		{"SELECT * FROM notes WHERE badcol = 1", nil, "column \"badcol\" not found", ""},
		{"SELECT 1 WHERE 1=?", nil, "missing value for query parameter ?1", ""},
		{"SELECT * FROM missing", nil, "table \"missing\" not found", ""},
	}

	for _, tc := range cases {
		_, _, err := st.Query(ctx, "test", tc.sql, tc.args, 0, 0)
		if err == nil {
			t.Fatalf("%s: expected an error", tc.sql)
		}
		msg := err.Error()
		if strings.Contains(msg, "SQL logic error") || strings.Contains(msg, "(1)") {
			t.Fatalf("%s: raw SQLite internals leaked: %q", tc.sql, msg)
		}
		if !strings.Contains(msg, tc.wantErr) {
			t.Fatalf("%s: expected message to contain %q, got %q", tc.sql, tc.wantErr, msg)
		}
		if tc.wantToken != "" && !strings.Contains(msg, tc.wantToken) {
			t.Fatalf("%s: expected offending token %q in message, got %q", tc.sql, tc.wantToken, msg)
		}
	}

	// Missing table is a not-found error with a query-safe message.
	_, _, err := st.Query(ctx, "test", "SELECT * FROM missing", nil, 0, 0)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing table should be ErrNotFound, got %v", err)
	}

	// Syntax errors are invalid requests.
	_, _, err = st.Query(ctx, "test", "SELECT * FROOOM notes", nil, 0, 0)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("syntax error should be ErrInvalid, got %v", err)
	}

	// The sanitized public message must not leak raw SQLite, but the original
	// error should still be available for server-side diagnostics.
	_, _, err = st.Query(ctx, "test", "SELECT * FROOOM notes", nil, 0, 0)
	var qe *QueryError
	if !errors.As(err, &qe) {
		t.Fatalf("expected a QueryError, got %T", err)
	}
	if qe.Cause() == nil {
		t.Fatalf("QueryError must preserve the original SQLite cause")
	}
	if !strings.Contains(qe.Cause().Error(), "SQL logic error") && !strings.Contains(qe.Cause().Error(), "(1)") {
		t.Fatalf("cause should be the raw SQLite error, got %q", qe.Cause().Error())
	}

	// Unwrap must expose both the original cause and the sentinel for errors.As/Is.
	unwrapped := qe.Unwrap()
	if len(unwrapped) != 2 {
		t.Fatalf("expected Unwrap to return 2 errors, got %d", len(unwrapped))
	}
	if unwrapped[0] != qe.Cause() {
		t.Fatalf("first unwrapped error should be the cause, got %v", unwrapped[0])
	}
	if !errors.Is(unwrapped[1], ErrInvalid) {
		t.Fatalf("second unwrapped error should be ErrInvalid, got %v", unwrapped[1])
	}
}

func TestOperationalFailuresAreNotQueryErrors(t *testing.T) {
	// Recognized input failures become sanitized QueryErrors.
	syntax := errors.New(`SQL logic error: near "FROOOM": syntax error (1)`)
	err := NewQueryError("SELECT 1", syntax)
	var qe *QueryError
	if !errors.As(err, &qe) {
		t.Fatalf("recognized syntax error should be a QueryError, got %T", err)
	}

	// Generic SQL-layer errors (primary result code 1) name the problem in the
	// message and are client-correctable even without a specific pattern.
	ambiguous := errors.New(`SQL logic error: ambiguous column name: id (1)`)
	err = NewQueryError("SELECT 1", ambiguous)
	if !errors.As(err, &qe) {
		t.Fatalf("generic SQL logic error should be a QueryError, got %T", err)
	}
	if !strings.Contains(err.Error(), "ambiguous column name") {
		t.Fatalf("generic SQL message should be preserved, got %q", err.Error())
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("generic SQL logic error should classify as ErrInvalid, got %v", err)
	}

	// Operational failures the client cannot correct (I/O, corruption, busy
	// timeouts) must stay internal: the original error is returned unwrapped
	// so it maps to internal_error, never a 400 query_error with a syntax hint.
	for _, raw := range []string{
		"database disk image is malformed",
		"database is locked (5) (SQLITE_BUSY)",
		"disk I/O error (10) (SQLITE_IOERR)",
		"context canceled",
	} {
		operational := errors.New(raw)
		err = NewQueryError("SELECT 1", operational)
		if errors.As(err, &qe) {
			t.Fatalf("operational failure %q must not classify as a QueryError, got %v", raw, err)
		}
		if !errors.Is(err, operational) {
			t.Fatalf("operational failure %q should return the original error, got %v", raw, err)
		}
		if errors.Is(err, ErrInvalid) {
			t.Fatalf("operational failure %q must not carry ErrInvalid, got %v", raw, err)
		}
	}
}

func TestAmbiguousColumnQueryIsInvalidRequest(t *testing.T) {
	st := openStore(t)
	mustCreateNotes(t, st)
	// A client-correctable error outside the specific pattern list must still
	// classify as an invalid request, not an internal error.
	_, _, err := st.Query(context.Background(), "test",
		"SELECT id FROM notes a JOIN notes b ON 1=1", nil, 0, 0)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("ambiguous column should classify as invalid request, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous column name") {
		t.Fatalf("expected the SQLite message preserved, got %q", msg)
	}
	if strings.Contains(msg, "SQL logic error") || strings.Contains(msg, "(1)") {
		t.Fatalf("raw SQLite framing leaked: %q", msg)
	}
}

func TestFilterErrorsUseFilterGuidance(t *testing.T) {
	st := openStore(t)
	mustCreateNotes(t, st)

	// Filter callers get WHERE-expression guidance, not SELECT/WITH guidance.
	_, err := st.Update(context.Background(), "test", "notes", "id =", nil,
		map[string]any{"title": "x"}, testEmbed)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed filter should classify as invalid request, got %v", err)
	}
	if !strings.Contains(err.Error(), "WHERE expression") {
		t.Fatalf("filter guidance should mention WHERE expressions, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "SELECT or WITH") {
		t.Fatalf("filter guidance must not point at SELECT/WITH statements, got %q", err.Error())
	}

	// Query callers keep statement-oriented guidance.
	_, _, qerr := st.Query(context.Background(), "test", "SELECT (", nil, 0, 0)
	if qerr == nil || !errors.Is(qerr, ErrInvalid) {
		t.Fatalf("incomplete query should classify as invalid request, got %v", qerr)
	}
	if !strings.Contains(qerr.Error(), "SELECT or WITH") {
		t.Fatalf("query guidance should mention SELECT/WITH statements, got %q", qerr.Error())
	}
}
