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
