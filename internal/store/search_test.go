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

	rows, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 0, 10, false)
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

	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "dolmen", 0, 10, false)
	if err != nil {
		t.Fatalf("fts after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected deleted row gone from fts, got %v", rows)
	}
	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "memory", 0, 10, false)
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
	fts, _, err := st.SearchFulltext(ctx, "test", "big", "row", 0, 10, false)
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
	fts, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 0, 10, false)
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
	if _, _, err := st.SearchFulltext(context.Background(), "test", "notes", "\"unterminated", 0, 10, false); err == nil || !errors.Is(err, ErrInvalid) {
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
	rows, truncated, err := st.SearchFulltext(ctx, "test", "bigsearch", "needle", 0, 200, false)
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
	rows, truncated, err := st.SearchFulltext(ctx, "test", "wide", "target", 0, 250, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) >= 200 || !truncated {
		t.Fatalf("wide-table labels must count against the budget: %d truncated=%v", len(rows), truncated)
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

	rows, truncated, err := st.SearchFulltext(ctx, "test", "ftspage", "needle", 0, 2, false)
	if err != nil {
		t.Fatalf("page 0: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("page 0 should return 2 rows and truncated=true: %d %v", len(rows), truncated)
	}

	rows, truncated, err = st.SearchFulltext(ctx, "test", "ftspage", "needle", 2, 2, false)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("page 1 should return 2 rows and truncated=true: %d %v", len(rows), truncated)
	}

	rows, truncated, err = st.SearchFulltext(ctx, "test", "ftspage", "needle", 4, 2, false)
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

	rows, truncated, err := st.SearchFulltext(ctx, "test", "ftssent", "needle", 0, 2, false)
	if err != nil {
		t.Fatalf("oversized sentinel row must not fail the page: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("expected 2 valid rows with truncated=true, got %d rows truncated=%v", len(rows), truncated)
	}
}
