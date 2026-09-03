package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("query count: %v", err)
	}
	if got := rows[0]["n"].(int64); got != 3 {
		t.Fatalf("expected 3 rows, got %d", got)
	}

	rows, _, err = st.Query(ctx, "test", "SELECT title FROM notes WHERE score > ? ORDER BY score DESC", []any{2})
	if err != nil {
		t.Fatalf("query filter: %v", err)
	}
	if len(rows) != 2 || rows[0]["title"] != "first note" || rows[1]["title"] != "second note" {
		t.Fatalf("unexpected rows: %v", rows)
	}

	if _, _, err := st.Query(ctx, "test", "DELETE FROM notes", nil); err == nil {
		t.Fatal("expected DELETE via query to be rejected")
	}
	if _, _, err := st.Query(ctx, "test", "INSERT INTO notes(title) VALUES('x')", nil); err == nil {
		t.Fatal("expected INSERT via query to be rejected")
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1; DROP TABLE notes", nil); err == nil {
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
		if _, _, err := st.Query(ctx, "test", q, nil); err != nil {
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
		if _, _, err := st.Query(ctx, "test", q, nil); err == nil {
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
	rows, _, err := st.Query(ctx, "test", "SELECT title FROM notes WHERE title = 'a;b'", nil)
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
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM opts", nil)
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
	rows, _, err := st.Query(ctx, "test", "SELECT flag, note FROM mixed ORDER BY id", nil)
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
	rows, _, err := st.Query(ctx, "test", "SELECT json_extract(v, '$') AS decoded FROM jf ORDER BY id", nil)
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
	rows, _, err := st.Query(ctx, "test", "SELECT emb FROM notes WHERE title = 'zero'", nil)
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
	rows, _, err := st.Query(ctx, "test", "SELECT n FROM prec", nil)
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
	rows, truncated, err := st.Query(ctx, "test", "SELECT * FROM cap", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1000 || !truncated {
		t.Fatalf("expected capped 1000 rows with truncated=true, got %d truncated=%v", len(rows), truncated)
	}
	rows, truncated, err = st.Query(ctx, "test", "SELECT count(*) AS n FROM cap", nil)
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
	rows, truncated, err := st.Query(ctx, "test", "SELECT * FROM exact", nil)
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
	rows, truncated, err := st.Query(ctx, "test", "SELECT v FROM bigvals", nil)
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
	if _, _, err := st.Query(ctx, "test", "SELECT zeroblob(34000000) AS b", nil); err == nil {
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
	if _, _, err := st.Query(ctx, "test", "SELECT 1e999 AS x", nil); err == nil {
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
	if _, _, err := st.Query(ctx, "test", "SELECT 1 AS a, 2 AS a", nil); err == nil {
		t.Fatal("expected duplicate column labels to be rejected")
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
	if _, _, err := st.Query(ctx, "test", "SELECT 1 AS \""+longAlias+"\"", nil); err == nil {
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
	if _, _, err := st.Query(ctx, "test", "SELECT (", nil); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed SQL to classify as invalid request, got %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1 WHERE 1=?", nil); err == nil || !errors.Is(err, ErrInvalid) {
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
	if _, _, err := st.Query(ctx, "test", "SELECT v FROM esc", nil); err == nil {
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
	if _, _, err := st.Query(ctx, "test", "SELECT json_extract('x', '$') AS v", nil); err == nil || !errors.Is(err, ErrInvalid) {
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
	rows, truncated, err := st.Query(ctx, "test", "SELECT v AS \""+alias+"\" FROM lbl2", nil)
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
	if _, _, err := st.Query(ctx, "test", "SELECT zeroblob(30000000) AS a, zeroblob(30000000) AS b", nil); err == nil {
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
	rows, _, err := st.Query(ctx, "test", "SELECT json_extract(payload, '$') AS v FROM jn", nil)
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
	rows, _, err := st.Query(ctx, "test", "SELECT json_extract(v, '$') AS val, typeof(json_extract(v, '$')) AS kind FROM js ORDER BY id", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for i, want := range []string{"true", "123", "null"} {
		if rows[i]["val"] != want || rows[i]["kind"] != "text" {
			t.Fatalf("row %d: string scalar changed type: %v", i, rows[i])
		}
	}
}

func TestQueryAllowsKeywordTableNames(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	// SQL keyword table names predate the keyword reservation, so simulate the
	// legacy tables through the namespace connection like the grandfathering
	// test; the guard must keep serving them.
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	for _, name := range []string{"left", "right", "full", "inner", "cross", "outer", "natural"} {
		sc := fmt.Sprintf(`{"name":%q,"version":1,"fields":[{"name":"v","type":"string"}]}`, name)
		if _, err := n.rw.ExecContext(ctx,
			`CREATE TABLE `+q(name)+` (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		if _, err := n.rw.ExecContext(ctx,
			`INSERT INTO _dolmen_tables(name, version, schema_json) VALUES(?,?,?)`, name, 1, sc); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
		if _, _, err := st.Query(ctx, "test", fmt.Sprintf("SELECT v FROM %s", name), nil); err != nil {
			t.Fatalf("query %q: %v", name, err)
		}
	}
	// Keyword table names can also appear after commas and in cross joins.
	if _, _, err := st.Query(ctx, "test", "SELECT left.v AS lv, right.v AS rv FROM left, right", nil); err != nil {
		t.Fatalf("comma-separated keyword tables: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT left.v FROM left CROSS JOIN right", nil); err != nil {
		t.Fatalf("cross join with keyword table: %v", err)
	}
}

func TestQueryRejectsReservedTables(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	cases := []struct {
		name  string
		query string
	}{
		{"_dolmen_tables", "SELECT * FROM _dolmen_tables"},
		{"_dolmen_migrations", "SELECT * FROM _dolmen_migrations"},
		{"_dolmen_idempotency", "SELECT * FROM _dolmen_idempotency"},
		{"sqlite_master", "SELECT * FROM sqlite_master"},
		{"sqlite_schema", "SELECT * FROM sqlite_schema"},
		{"sqlite_temp_master", "SELECT * FROM sqlite_temp_master"},
		{"__fts virtual", "SELECT * FROM notes__fts"},
		{"__fts data", "SELECT * FROM notes__fts_data"},
		{"__fts idx", "SELECT * FROM notes__fts_idx"},
		{"__fts content", "SELECT * FROM notes__fts_content"},
		{"__fts docsize", "SELECT * FROM notes__fts_docsize"},
		{"__fts config", "SELECT * FROM notes__fts_config"},
		{"aliased internal", "SELECT * FROM _dolmen_tables t"},
		{"qualified internal", "SELECT * FROM main._dolmen_tables"},
		{"temp internal", "SELECT * FROM temp._dolmen_delete_ids"},
		{"subquery", "SELECT 1 WHERE EXISTS (SELECT 1 FROM _dolmen_tables)"},
		{"select-list subquery", "SELECT (SELECT count(*) FROM _dolmen_tables) FROM notes"},
		{"limit subquery", "SELECT * FROM notes LIMIT (SELECT count(*) FROM _dolmen_tables)"},
		{"cte", "WITH cte AS (SELECT * FROM _dolmen_tables) SELECT * FROM cte"},
		{"cte qualified", "WITH cte AS (SELECT * FROM notes) SELECT * FROM cte, _dolmen_tables"},
		{"join", "SELECT * FROM notes JOIN _dolmen_tables d ON notes.id = d.rowid"},
		{"compound", "SELECT * FROM notes EXCEPT SELECT * FROM sqlite_master"},
		{"quoted reserved", `SELECT * FROM "_dolmen_tables"`},
		// A quoted identifier containing a dot is one name; without a matching
		// CTE its reserved-looking tail still rejects.
		{"dotted quoted reserved", `SELECT * FROM "c._dolmen_tables"`},
		{"bracketed reserved", "SELECT * FROM [sqlite_master]"},
		{"parenthesized reserved", "SELECT * FROM (_dolmen_tables)"},
		{"parenthesized join list reserved", "SELECT * FROM (notes JOIN _dolmen_tables ON notes.id = _dolmen_tables.rowid)"},
		{"pragma table list", "SELECT * FROM pragma_table_list()"},
		{"pragma table info reserved", "SELECT * FROM pragma_table_info('_dolmen_tables')"},
		{"pragma table xinfo reserved", "SELECT * FROM pragma_table_xinfo('sqlite_master')"},
		{"pragma computed arg", "SELECT * FROM pragma_table_info(char(95) || 'dolmen_tables')"},
		{"values compound internal", "WITH c AS (VALUES(1) UNION ALL SELECT rowid FROM _dolmen_tables) SELECT * FROM c"},
		{"values compound sqlite", "WITH c AS (VALUES(1) UNION ALL SELECT rowid FROM sqlite_master) SELECT * FROM c"},
		{"window alias join reserved", "SELECT d.name FROM notes window JOIN _dolmen_tables d"},
		{"values subquery internal", "SELECT (VALUES('safe') UNION ALL SELECT schema_json FROM _dolmen_tables LIMIT 1 OFFSET 1) FROM notes"},
		{"cte scope leak", "SELECT (WITH _dolmen_tables(x) AS (VALUES(1)) SELECT x FROM _dolmen_tables), name FROM _dolmen_tables"},
		{"pragma arg reserved", "SELECT * FROM pragma_table_info('pragma_table_list')"},
		{"dbstat", "SELECT * FROM dbstat"},
		{"nested expression bypass", "SELECT coalesce((SELECT 1), 0), schema_json FROM _dolmen_tables"},
		// A $ inside an identifier must not split it: otherwise the scanner
		// sees a fake FROM, treats the real FROM as a table named "from", and
		// the reserved table slips through as its alias.
		{"dollar alias bypass", "SELECT schema_json, 1 AS x$from FROM _dolmen_tables"},
		// expr IN table is shorthand for expr IN (SELECT * FROM table), so the
		// bare-table operand must be validated like a FROM factor or it leaks a
		// boolean oracle over internal tables.
		{"bare table in oracle", "SELECT ('notes','secret',NULL,NULL,NULL) IN _dolmen_idempotency"},
		{"bare table in", "SELECT 1 WHERE 1 IN _dolmen_tables"},
		{"bare table not in", "SELECT 1 WHERE 1 NOT IN sqlite_master"},
		{"bare table in paren group", "SELECT (1 IN _dolmen_tables) FROM notes"},
		{"bare table in qualified", "SELECT 1 WHERE 1 IN main._dolmen_tables"},
		{"bare table in pragma bare", "SELECT 1 WHERE 1 IN pragma_table_list"},
		{"colon param bypass", "SELECT 1 AS x:from FROM _dolmen_tables"},
		{"at param bypass", "SELECT 1 AS x@from FROM _dolmen_tables"},
		// Tcl-style :: suffixes are part of a parameter name in SQLite.
		{"tcl dollar param bypass", "SELECT schema_json, $x::from FROM _dolmen_tables"},
		{"tcl colon param bypass", "SELECT schema_json, :x::from FROM _dolmen_tables"},
		{"tcl at param bypass", "SELECT schema_json, @x::from FROM _dolmen_tables"},
		{"tcl bare suffix bypass", "SELECT schema_json, $::from FROM _dolmen_tables"},
		{"paren param bypass", "SELECT schema_json, $x(from) FROM _dolmen_tables"},
		{"paren punct param bypass", "SELECT schema_json, $x(a,from) FROM _dolmen_tables"},
		{"paren colon param bypass", "SELECT schema_json, $x(a:from) FROM _dolmen_tables"},
		{"paren open param bypass", "SELECT schema_json, $x(a(b) FROM _dolmen_tables"},
		{"hash param bypass", "SELECT schema_json, #from FROM _dolmen_tables"},
		// Go's ToLower maps İ to i, but SQLite folds identifiers ASCII-only,
		// so a CTE whose name lowercases (in Go) onto an internal table's name
		// must not shadow it.
		{"i-dot cte shadow", "WITH _dolmen_İdempotency(x) AS (VALUES(1)) SELECT * FROM _dolmen_idempotency"},
		{"bare table in dbstat fn", "SELECT 1 WHERE 1 IN dbstat()"},
		// The long-s fold orbit (ſ equals s under Unicode simple folding)
		// must not make the alias ſrom act as the FROM keyword.
		{"long-s alias fold", "SELECT 1 AS ſrom FROM _dolmen_tables"},
		// Non-ASCII names cannot be created as tables, and without a matching
		// CTE they resolve to nothing, so they are rejected like any other
		// non-user table.
		{"non-ascii table", "SELECT * FROM 日本語"},
		{"excessive table paren nesting", "SELECT * FROM " + strings.Repeat("(", maxTableParens+1) + "notes" + strings.Repeat(")", maxTableParens+1)},
		{"excessive statement nesting", "SELECT * FROM " + strings.Repeat("(SELECT * FROM ", maxStmtDepth+1) + "notes" + strings.Repeat(")", maxStmtDepth+1)},
		{"excessive query length", "SELECT * FROM notes WHERE x = '" + strings.Repeat("x", MaxQueryRunes) + "'"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := st.Query(ctx, "test", tc.query, nil)
			if err == nil {
				t.Fatalf("expected reserved table query to be rejected")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestQueryAllowsUserTables(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.CreateTable(ctx, "test", "users", []schema.Field{
		{Name: "name", Type: schema.String},
	}); err != nil {
		t.Fatalf("create users: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "sides", []schema.Field{
		{Name: "leg_a", Type: schema.Number},
		{Name: "leg_b", Type: schema.Number},
	}); err != nil {
		t.Fatalf("create sides: %v", err)
	}

	ok := []string{
		"SELECT * FROM notes",
		"SELECT * FROM notes n",
		"SELECT n.id FROM notes n JOIN users u ON n.id = u.id",
		"SELECT * FROM notes WHERE id IN (SELECT id FROM users)",
		"SELECT * FROM notes WHERE EXISTS (SELECT 1 FROM users u WHERE u.id = notes.id)",
		"WITH cte AS (SELECT * FROM notes) SELECT * FROM cte",
		"SELECT id FROM notes EXCEPT SELECT id FROM users",
		"SELECT id FROM notes UNION SELECT id FROM users",
		"SELECT '_dolmen_tables' AS lit FROM notes",
		"SELECT * FROM notes /* _dolmen_tables */",
		"SELECT * FROM notes WHERE title = '-- _dolmen_tables'",
		"SELECT count(*) FROM (SELECT * FROM notes)",
		"SELECT * FROM (notes)",
		"SELECT n.id FROM (notes n JOIN users u ON n.id = u.id)",
		"WITH cte AS (VALUES (1, 'x')) SELECT * FROM cte",
		"WITH cte(x, y) AS (VALUES (1, 'x'), (2, 'y')) SELECT * FROM cte",
		"WITH c AS (VALUES(1) UNION ALL SELECT id FROM notes) SELECT * FROM c",
		"SELECT * FROM (VALUES (1, 2)) AS t",
		"SELECT * FROM pragma_table_info('notes')",
		"SELECT * FROM pragma_table_info('notes', 'main')",
		"WITH c AS MATERIALIZED (SELECT * FROM notes) SELECT * FROM c",
		"WITH c AS NOT MATERIALIZED (SELECT * FROM notes) SELECT * FROM c",
		"SELECT sum(score) OVER win FROM notes WINDOW win AS (ORDER BY id)",
		"SELECT sum(score) OVER 'win' FROM notes WINDOW 'win' AS (ORDER BY id)",
		"WITH sqlite_master(x) AS (VALUES(1)) SELECT x FROM sqlite_master",
		"WITH _dolmen_tables(x) AS (VALUES(1)) SELECT x FROM _dolmen_tables",
		"SELECT n.id FROM notes n JOIN sides s ON n.id = s.leg_a",
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x < 3) SELECT * FROM c",
		"SELECT (VALUES(1) UNION ALL SELECT id FROM notes LIMIT 1) FROM notes",
		"WITH a AS (SELECT x FROM _dolmen_tables), _dolmen_tables(x) AS (VALUES(7)) SELECT * FROM a",
		"WITH pragma_table_list(x) AS (VALUES(1)) SELECT * FROM pragma_table_list",
		`SELECT "my alias".id FROM notes "my alias"`,
		"SELECT * FROM notes WHERE title = (((('x'))))",
		"SELECT * FROM notes WHERE title = (SELECT 'x' FROM (VALUES(1)))",
		"SELECT * FROM (((notes)))",
		"SELECT * FROM 'notes'",
		"SELECT * FROM main.'notes'",
		"SELECT * FROM notes AS 'n'",
		"SELECT * FROM (VALUES (1)) AS 'v'",
		`SELECT "my alias".id FROM notes 'my alias'`,
		"WITH 'c'(x) AS (VALUES(1)) SELECT * FROM 'c'",
		// SQLite treats non-ASCII characters as identifier characters, so
		// Unicode CTE names and aliases must tokenize as identifiers.
		"WITH 日本語(x) AS (VALUES(1)) SELECT * FROM 日本語",
		"WITH résumé AS (SELECT id FROM notes) SELECT * FROM résumé",
		"SELECT * FROM notes AS 日本語",
		"SELECT 日本語.id FROM notes 日本語",
		// '$' continues an identifier (SQLite IdChar), so keywords cannot be
		// recognized inside dollar-containing names.
		"SELECT 1 AS x$from FROM notes",
		"WITH c$1 AS (SELECT id FROM notes) SELECT * FROM c$1",
		"SELECT n$x.id FROM notes n$x",
		// Keyword matching is ASCII-only: ſrom is a plain alias to SQLite,
		// never the FROM keyword.
		"SELECT 1 AS ſrom FROM notes",
		// Identifier folding is ASCII-only too: a CTE named with İ keeps that
		// spelling and references to it resolve to the CTE.
		"WITH _dolmen_İdempotency(x) AS (VALUES(1)) SELECT * FROM _dolmen_İdempotency",
		// A quoted name containing a dot is one identifier, so the whole name
		// is matched against the CTE scope before any schema split.
		`WITH "c._dolmen_tables"(x) AS (VALUES(1)) SELECT * FROM "c._dolmen_tables"`,
		// Form feed is SQLite whitespace, so it separates tokens like a space.
		"WITH\fc(x) AS (VALUES(1)) SELECT * FROM c",
		"WITH c AS\f(VALUES(1)) SELECT * FROM c",
		"SELECT\fcount(*) AS n FROM notes",
		// Bare-table IN over user data stays allowed.
		"WITH c(x) AS (VALUES(1)) SELECT 1 WHERE 1 IN c",
		"SELECT id FROM notes WHERE id IN (SELECT id FROM notes)",
		// MaxQueryRunes counts characters, matching JSON Schema maxLength, so a
		// query whose UTF-8 encoding is larger than the limit in bytes but within
		// it in characters is accepted.
		"SELECT * FROM notes WHERE title = '" + strings.Repeat("é", MaxQueryRunes-100) + "' LIMIT 0",
	}

	for _, q := range ok {
		t.Run(q, func(t *testing.T) {
			if _, _, err := st.Query(ctx, "test", q, nil); err != nil {
				t.Fatalf("expected query to be allowed: %v", err)
			}
		})
	}
}

// TestQueryValidatorCTEScale guards the validator against quadratic behavior on
// long sequential CTE lists: each CTE body must not re-copy the names of its
// siblings. A list this size validates in well under a second when linear, but
// takes minutes when each body clones the forward-name map.
func TestQueryValidatorCTEScale(t *testing.T) {
	var b strings.Builder
	b.WriteString("WITH ")
	for i := 0; i < 30000; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "c%d AS (VALUES(1))", i)
	}
	b.WriteString(" SELECT * FROM notes LIMIT 0")

	start := time.Now()
	if err := validateQueryTables(b.String(), nil); err != nil {
		t.Fatalf("expected sequential CTEs to validate: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("sequential CTE validation should stay linear, took %v", d)
	}
}

// TestQueryTokenizesVariablesAtomically checks that SQLite variable tokens
// (:name, @name, $name, ?NNN) are consumed as one unit, so a keyword inside a
// parameter name can never be mistaken for a clause boundary.
func TestQueryTokenizesVariablesAtomically(t *testing.T) {
	ok := []string{
		"SELECT title FROM notes WHERE title = :name OR title = @name OR title = $name",
		"SELECT title FROM notes WHERE title = ?1",
		"SELECT :from AS f, @from AS g, $from AS h FROM notes",
		"SELECT $x::ns::y AS v, :x::from AS w, @x::from AS z FROM notes",
		"SELECT $::from AS v FROM notes",
		"SELECT $x(with) AS v, :x(with) AS w, @x(with) AS x FROM notes",
		"SELECT $x::y(with) AS v, $x() AS w FROM notes",
		"SELECT $x(a,from) AS v, $x(a.b) AS w, $x('a;b') AS z FROM notes",
		"SELECT $x(a:from) AS v, $x(a(b) AS w FROM notes",
		"SELECT #from AS v, #x(a,from) AS w, #x::y AS z FROM notes",
		// A user table in bare-table IN position passes the guard (SQLite
		// checks column cardinality itself).
		"SELECT 1 WHERE 1 IN notes",
	}
	for _, q := range ok {
		if err := validateQueryTables(q, nil); err != nil {
			t.Errorf("expected %q to validate: %v", q, err)
		}
	}

	rejected := []string{
		"SELECT schema_json, 1 AS x$from FROM _dolmen_tables",
		"SELECT 1 AS x:from FROM _dolmen_tables",
		"SELECT 1 AS x@from FROM _dolmen_tables",
		"SELECT 1 AS x$from, schema_json FROM _dolmen_tables UNION SELECT 1, 2 FROM notes",
	}
	for _, q := range rejected {
		if err := validateQueryTables(q, nil); err == nil {
			t.Errorf("expected %q to be rejected", q)
		}
	}
}

// TestQueryAllowsGrandfatheredReservedNames covers tables created before
// pragma_*/dbstat were reserved: they remain registered user data, so the
// guard must keep serving them while rejecting new reserved-named tables.
func TestQueryAllowsGrandfatheredReservedNames(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	for _, name := range []string{"dbstat", "pragma_notes"} {
		if _, err := n.rw.ExecContext(ctx,
			`CREATE TABLE `+q(name)+` (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		sc := fmt.Sprintf(`{"name":%q,"version":1,"fields":[{"name":"v","type":"string"}]}`, name)
		if _, err := n.rw.ExecContext(ctx,
			`INSERT INTO _dolmen_tables(name, version, schema_json) VALUES(?,?,?)`,
			name, 1, sc); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if _, err := st.CreateTable(ctx, "test", "dbstat", []schema.Field{{Name: "v", Type: schema.String}}); err == nil {
		t.Fatal("expected new reserved-named table creation to be rejected")
	}
	for _, name := range []string{"dbstat", "pragma_notes"} {
		if _, _, err := st.Query(ctx, "test", "SELECT v FROM "+name, nil); err != nil {
			t.Fatalf("query grandfathered %s: %v", name, err)
		}
	}
	// The registry cannot smuggle internal tables: those names were never
	// creatable, so they stay rejected.
	if _, _, err := st.Query(ctx, "test", "SELECT * FROM _dolmen_tables", nil); err == nil {
		t.Fatal("expected internal registry to stay rejected")
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
		_, _, err := st.Query(ctx, "test", tc.sql, tc.args)
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
	_, _, err := st.Query(ctx, "test", "SELECT * FROM missing", nil)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing table should be ErrNotFound, got %v", err)
	}

	// Syntax errors are invalid requests.
	_, _, err = st.Query(ctx, "test", "SELECT * FROOOM notes", nil)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("syntax error should be ErrInvalid, got %v", err)
	}

	// The sanitized public message must not leak raw SQLite, but the original
	// error should still be available for server-side diagnostics.
	_, _, err = st.Query(ctx, "test", "SELECT * FROOOM notes", nil)
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
