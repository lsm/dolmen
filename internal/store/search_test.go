package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestFulltextSearchAndDeleteCascade(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	rows, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "first note" {
		t.Fatalf("unexpected fts results: %v", rows)
	}

	res, err := st.Delete(ctx, "test", "notes", "done = 1", nil, DeleteOptions{})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deleted, got %d", res.Deleted)
	}

	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "dolmen", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("fts after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected deleted row gone from fts, got %v", rows)
	}
	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "memory", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("fts survivor: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "second note" {
		t.Fatalf("unexpected survivor: %v", rows)
	}
}

func TestLargeDeleteUsesNoInParameterLists(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "big", []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for batch := 0; batch < 2; batch++ {
		records := make([]map[string]any, 0, 600)
		for i := 0; i < 600; i++ {
			records = append(records, map[string]any{"title": fmt.Sprintf("row %d-%d", batch, i)})
		}
		if _, err := st.Insert(ctx, "test", "big", records, testEmbed); err != nil {
			t.Fatalf("insert batch %d: %v", batch, err)
		}
	}

	res, err := st.Delete(ctx, "test", "big", "1=1", nil, DeleteOptions{Confirm: true})
	if err != nil {
		t.Fatalf("large delete: %v", err)
	}
	if res.Deleted != 1200 {
		t.Fatalf("expected 1200 deleted, got %d", res.Deleted)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM big", nil, 0, 0)
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if got := rows[0]["n"].(int64); got != 0 {
		t.Fatalf("expected 0 rows after delete, got %d", got)
	}
	fts, _, err := st.SearchFulltext(ctx, "test", "big", "row", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("fts after delete: %v", err)
	}
	if len(fts) != 0 {
		t.Fatalf("expected fts empty after delete, got %d rows", len(fts))
	}
}

func TestDeleteFilterEvaluatedOnce(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	res, err := st.Delete(ctx, "test", "notes", "EXISTS (SELECT 1 FROM notes__fts)", nil, DeleteOptions{})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if res.Deleted != 3 {
		t.Fatalf("filter must be evaluated once: expected 3 deleted, got %d", res.Deleted)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil, 0, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatalf("base rows must be deleted alongside the index: %v", rows)
	}
	fts, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(fts) != 0 {
		t.Fatalf("expected empty search results, got %v", fts)
	}
}

func TestMalformedDeleteFilterIsInvalidRequest(t *testing.T) {
	st := openStore(t)
	mustCreateNotes(t, st)
	_, err := st.Delete(context.Background(), "test", "notes", "id =", nil, DeleteOptions{})
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed filter to classify as invalid request, got %v", err)
	}
}

func TestDeleteFilterSemicolonInsideQuotesAllowed(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "a;b", "body": "semicolon title"},
		{"title": "plain", "body": "plain title"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := st.Delete(ctx, "test", "notes", "title = 'a;b'", nil, DeleteOptions{})
	if err != nil {
		t.Fatalf("delete with semicolon in literal: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deleted, got %d", res.Deleted)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil, 0, 0)
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if rows[0]["n"].(int64) != 1 {
		t.Fatalf("expected 1 survivor, got %v", rows)
	}
}

func TestDeleteFilterMultipleStatementsRejected(t *testing.T) {
	st := openStore(t)
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)
	_, err := st.Delete(context.Background(), "test", "notes", "title = 'a;b'; DROP TABLE notes", nil, DeleteOptions{})
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected multi-statement filter to be rejected, got %v", err)
	}
}

func TestMalformedFTSQueryIsInvalidRequest(t *testing.T) {
	st := openStore(t)
	mustCreateNotes(t, st)
	if _, _, err := st.SearchFulltext(context.Background(), "test", "notes", "\"unterminated", 0, 10, false, "", nil); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed FTS syntax to classify as invalid request, got %v", err)
	}
}

func TestSearchByteBudget(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "bigsearch", []schema.Field{
		{Name: "v", Type: schema.Text, Fulltext: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	chunk := strings.Repeat("needle ", (12<<20)/7)
	for i := 0; i < 4; i++ {
		if _, err := st.Insert(ctx, "test", "bigsearch", []map[string]any{{"v": chunk}}, testEmbed); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, truncated, err := st.SearchFulltext(ctx, "test", "bigsearch", "needle", 0, 200, false, "", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("search byte budget should cap at 2 of 4 12MiB rows: %d truncated=%v", len(rows), truncated)
	}
}

func TestSearchLabelBudget(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	needle := strings.Repeat("f", 64)
	fields := []schema.Field{{Name: needle, Type: schema.String, Fulltext: true}, {Name: "payload", Type: schema.Text}}
	long := strings.Repeat("c", 60)

	for i := 0; i < MaxFieldsPerTable-2; i++ {
		fields = append(fields, schema.Field{Name: long + fmt.Sprint(i), Type: schema.String})
	}
	if _, err := st.CreateTable(ctx, "test", "wide", fields); err != nil {
		t.Fatalf("create: %v", err)
	}
	big := strings.Repeat("p", 160<<10)
	records := make([]map[string]any, 0, 250)
	for i := 0; i < 250; i++ {
		records = append(records, map[string]any{needle: "target", "payload": big})
	}
	if _, err := st.Insert(ctx, "test", "wide", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, truncated, err := st.SearchFulltext(ctx, "test", "wide", "target", 0, 250, false, "", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) >= 200 || !truncated {
		t.Fatalf("wide-table labels must count against the budget: %d truncated=%v", len(rows), truncated)
	}
}

func TestSearchFulltextFilter(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	// Filter to rows whose title matches a bound parameter.
	rows, _, err := st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, "title = ?", []any{"first note"})
	if err != nil {
		t.Fatalf("filtered fts: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "first note" {
		t.Fatalf("expected one filtered hit, got %v", rows)
	}

	// Filter on a numeric metadata column.
	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, "score >= 3", nil)
	if err != nil {
		t.Fatalf("numeric filter fts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 hits with score >= 3, got %v", rows)
	}

	// The filter sees the base table under its own name, like search_vector's.
	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, "notes.done = 1", nil)
	if err != nil {
		t.Fatalf("qualified filter fts: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "first note" {
		t.Fatalf("expected the done row, got %v", rows)
	}

	// Filter with no matches returns empty, not an error.
	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, "title = ?", []any{"missing"})
	if err != nil {
		t.Fatalf("zero-match filter fts: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 hits for missing filter, got %v", rows)
	}

	// A null bind argument is accepted and simply matches nothing here.
	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, "title = ?", []any{nil})
	if err != nil {
		t.Fatalf("null bind filter fts: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 hits for null title bind, got %v", rows)
	}

	// Numbered placeholders (?NNN) bind from args with the same numbering the
	// filter has standalone, like search_vector's filter — the internal MATCH
	// and pagination parameters must not shift them.
	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, "score >= ?1", []any{3})
	if err != nil {
		t.Fatalf("numbered bind filter fts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 hits with score >= 3 via ?1, got %v", rows)
	}
	rows, truncated, err := st.SearchFulltext(ctx, "test", "notes", "note", 0, 1, false, "score >= ?1", []any{1})
	if err != nil {
		t.Fatalf("numbered bind pagination fts: %v", err)
	}
	if len(rows) != 1 || !truncated {
		t.Fatalf("pagination must bind correctly alongside ?1: %d rows truncated=%v", len(rows), truncated)
	}

	// Malicious/invalid filters are rejected like search_vector's filter.
	if _, _, err := st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, "1=1; DROP TABLE notes", nil); err == nil {
		t.Fatal("expected semicolon in filter to be rejected")
	}
	if _, _, err := st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, "title = ? AND score > ?", []any{"first note"}); err == nil {
		t.Fatal("expected too few filter args to be rejected")
	}
}

// A base-table field named rank (allowed when not fulltext) must not collide
// with the FTS rank used for ordering — the filter's rank is the table's.
func TestSearchFulltextFilterWithRankField(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "ftsrnk", []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true},
		{Name: "rank", Type: schema.Number},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "ftsrnk", []map[string]any{
		{"title": "needle", "rank": 1},
		{"title": "needle", "rank": 2},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.SearchFulltext(ctx, "test", "ftsrnk", "needle", 0, 10, false, "rank >= 2", nil)
	if err != nil {
		t.Fatalf("filtered fts over a rank field: %v", err)
	}
	if len(rows) != 1 || rows[0]["rank"] != int64(2) {
		t.Fatalf("expected the rank=2 row, got %v", rows)
	}
}

// Failures must be attributed to the expression that caused them: a malformed
// MATCH with a valid filter stays an FTS-query error, and a malformed filter
// with a valid MATCH reports the filter.
func TestSearchFulltextFilterErrorClassification(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	_, _, err := st.SearchFulltext(ctx, "test", "notes", "\"unterminated", 0, 10, false, "score > 0", nil)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed MATCH to classify as invalid request, got %v", err)
	}
	if strings.Contains(err.Error(), "the filter must be") {
		t.Fatalf("malformed MATCH must keep the FTS-query error wording, got %v", err)
	}

	_, _, err = st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, "bogus =", nil)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed filter to classify as invalid request, got %v", err)
	}
	if !strings.Contains(err.Error(), "the filter must be") {
		t.Fatalf("malformed filter must carry the filter wording, got %v", err)
	}

	// A filter that compiles but fails while evaluating a real row is still a
	// filter failure, not a MATCH failure.
	_, _, err = st.SearchFulltext(ctx, "test", "notes", "note", 0, 10, false, `json_extract(title, '$.x') = 1`, nil)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected runtime filter failure to classify as invalid request, got %v", err)
	}
	if !strings.Contains(err.Error(), "the filter must be") {
		t.Fatalf("runtime filter failure must carry the filter wording, got %v", err)
	}
}

// The filtered query must check the filter with one primary-key lookup per
// FTS hit, not by scanning (or materializing the matches of) the base table —
// an unselective filter must not turn a selective MATCH into O(table).
func TestSearchFulltextFilterPlanLooksUpRowsPerHit(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	rows, err := n.ro.QueryContext(ctx, `EXPLAIN QUERY PLAN `+fulltextFilterStmt("notes", "score >= 3", 0), "note", 10, 0)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused any
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	seek := "SEARCH notes USING INTEGER PRIMARY KEY (rowid=?)"
	found := false
	for _, d := range details {
		if d == seek {
			found = true
		}
		// The FTS side renders as "SCAN notes__fts VIRTUAL TABLE INDEX" —
		// that prefix must not hide a scan of the base table itself.
		if strings.HasPrefix(d, "SCAN notes") && !strings.HasPrefix(d, "SCAN notes__fts") {
			t.Fatalf("filtered search must not scan the base table, plan: %v", details)
		}
	}
	if !found {
		t.Fatalf("filtered search must probe the base table by primary key per FTS hit, plan: %v", details)
	}
}

// The filter must restrict the result set without reordering it: a filtered
// page is exactly the unfiltered ranking with non-matching rows removed, and
// offset/limit/truncated then page within that restricted set.
func TestSearchFulltextFilterKeepsRankOrderAndPagination(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "ftsfilt", []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true},
		{Name: "keep", Type: schema.Boolean},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Varied needle density so BM25 ranks the rows differently.
	records := []map[string]any{
		{"title": "needle", "keep": false},
		{"title": "needle needle", "keep": true},
		{"title": "needle needle needle", "keep": false},
		{"title": "filler needle", "keep": true},
		{"title": "filler filler needle needle", "keep": true},
	}
	if _, err := st.Insert(ctx, "test", "ftsfilt", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	all, _, err := st.SearchFulltext(ctx, "test", "ftsfilt", "needle", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("unfiltered fts: %v", err)
	}
	if len(all) != len(records) {
		t.Fatalf("expected all %d rows unfiltered, got %v", len(records), all)
	}
	var want []any
	for _, r := range all {
		if r["keep"] == true {
			want = append(want, r["title"])
		}
	}

	rows, truncated, err := st.SearchFulltext(ctx, "test", "ftsfilt", "needle", 0, 10, false, "keep = 1", nil)
	if err != nil {
		t.Fatalf("filtered fts: %v", err)
	}
	var got []any
	for _, r := range rows {
		got = append(got, r["title"])
	}
	if len(got) != len(want) {
		t.Fatalf("expected the keep=true subset %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("filter must not reorder results: expected %v, got %v", want, got)
		}
	}
	if truncated {
		t.Fatalf("no pagination expected within one page, got truncated=%v", truncated)
	}

	// Pagination and truncated still work within the filtered set.
	rows, truncated, err = st.SearchFulltext(ctx, "test", "ftsfilt", "needle", 0, 2, false, "keep = 1", nil)
	if err != nil {
		t.Fatalf("filtered page 0: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("filtered page 0 should return 2 rows with truncated=true: %d %v", len(rows), truncated)
	}
	if rows[0]["title"] != want[0] || rows[1]["title"] != want[1] {
		t.Fatalf("filtered page 0 must lead with the best ranked keep=true rows: %v", rows)
	}
	rows, truncated, err = st.SearchFulltext(ctx, "test", "ftsfilt", "needle", 2, 2, false, "keep = 1", nil)
	if err != nil {
		t.Fatalf("filtered page 1: %v", err)
	}
	if len(rows) != 1 || truncated {
		t.Fatalf("filtered page 1 should return 1 row with truncated=false: %d %v", len(rows), truncated)
	}
}

func TestFulltextPaginationAndTruncatedFlag(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "ftspage", []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	records := make([]map[string]any, 0, 5)
	for i := 0; i < 5; i++ {
		records = append(records, map[string]any{"title": "needle"})
	}
	if _, err := st.Insert(ctx, "test", "ftspage", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, truncated, err := st.SearchFulltext(ctx, "test", "ftspage", "needle", 0, 2, false, "", nil)
	if err != nil {
		t.Fatalf("page 0: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("page 0 should return 2 rows and truncated=true: %d %v", len(rows), truncated)
	}

	rows, truncated, err = st.SearchFulltext(ctx, "test", "ftspage", "needle", 2, 2, false, "", nil)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("page 1 should return 2 rows and truncated=true: %d %v", len(rows), truncated)
	}

	rows, truncated, err = st.SearchFulltext(ctx, "test", "ftspage", "needle", 4, 2, false, "", nil)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(rows) != 1 || truncated {
		t.Fatalf("page 2 should return 1 row with truncated=false: %d %v", len(rows), truncated)
	}
}

func TestFulltextSentinelRowNeverFailsThePage(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "ftssent", []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true},
		{Name: "payload", Type: schema.Text},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "ftssent", []map[string]any{
		{"title": "needle", "payload": "one"},
		{"title": "needle", "payload": "two"},
		{"title": "needle", "payload": "three"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The (limit+1)th match carries a blob beyond the response budget: it is
	// only a look-ahead for truncated, so it must never be materialized.
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	if _, err := n.rw.ExecContext(ctx,
		`UPDATE ftssent SET payload = randomblob(34000000) WHERE title = 'needle' AND payload = 'three'`); err != nil {
		t.Fatalf("out-of-band write: %v", err)
	}

	rows, truncated, err := st.SearchFulltext(ctx, "test", "ftssent", "needle", 0, 2, false, "", nil)
	if err != nil {
		t.Fatalf("oversized sentinel row must not fail the page: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("expected 2 valid rows with truncated=true, got %d rows truncated=%v", len(rows), truncated)
	}
}
