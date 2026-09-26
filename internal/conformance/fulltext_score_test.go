package conformance

import (
	"testing"
)

func seedScoredNotes(t *testing.T, h *harness) {
	t.Helper()
	h.seedTable("fts", "notes", []map[string]any{
		{"name": "body", "type": "text", "fulltext": true},
		{"name": "tag", "type": "string"},
	})
	h.mustHTTP("insert", map[string]any{"namespace": "fts", "table": "notes", "records": []map[string]any{
		{"body": "pump pump pump overheating pump", "tag": "hot"},
		{"body": "the pump is quiet and the fan is loud and the room is cold", "tag": "cold"},
		{"body": "fan only, nothing about the other thing", "tag": "cold"},
	}})
}

func scoredResults(t *testing.T, h *harness, body map[string]any) []map[string]any {
	t.Helper()
	data := h.mustHTTP("search_fulltext", body)
	assertJSONEqual(t, "search_fulltext over MCP vs HTTP", h.mustMCP("search_fulltext", body), data)
	list, _ := data["results"].([]any)
	if len(list) == 0 {
		t.Fatalf("search returned nothing: %v", data)
	}
	out := make([]map[string]any, len(list))
	for i, r := range list {
		row, _ := r.(map[string]any)
		if row == nil {
			t.Fatalf("result %d is not an object: %v", i, r)
		}
		out[i] = row
	}
	return out
}

func TestFulltextSearchScoresEveryHit(t *testing.T) {
	h := newHarness(t)
	seedScoredNotes(t, h)

	rows := scoredResults(t, h, map[string]any{"namespace": "fts", "table": "notes", "query": "pump"})
	if len(rows) != 2 {
		t.Fatalf("query pump matched %d rows, want 2", len(rows))
	}
	first := float(t, "_score", rows[0]["_score"])
	second := float(t, "_score", rows[1]["_score"])
	if first <= 0 || second <= 0 {
		t.Fatalf("every hit needs a positive relevance score, got %v and %v", first, second)
	}
	if first < second {
		t.Fatalf("_score must rank with the result order, higher first: %v then %v", first, second)
	}
	if rows[0]["body"] != "pump pump pump overheating pump" {
		t.Fatalf("the denser match must rank first, got %v", rows[0]["body"])
	}

	filtered := scoredResults(t, h, map[string]any{"namespace": "fts", "table": "notes", "query": "pump", "filter": "tag = ?", "args": []any{"cold"}})
	if len(filtered) != 1 {
		t.Fatalf("a filtered search matched %d rows, want 1", len(filtered))
	}
	if got := float(t, "_score", filtered[0]["_score"]); got <= 0 {
		t.Fatalf("a filtered hit carries no score: %v", got)
	}

	paged := scoredResults(t, h, map[string]any{"namespace": "fts", "table": "notes", "query": "pump", "limit": 1, "offset": 1})
	if len(paged) != 1 {
		t.Fatalf("the second page holds %d rows, want 1", len(paged))
	}
	if got := float(t, "_score", paged[0]["_score"]); got <= 0 {
		t.Fatalf("a hit on a later page carries no score: %v", got)
	}
	if paged[0]["body"] != rows[1]["body"] {
		t.Fatalf("paging changed the order: %v then %v", rows[1]["body"], paged[0]["body"])
	}
}

func TestFulltextScoreIsNotAStoredField(t *testing.T) {
	h := newHarness(t)
	seedScoredNotes(t, h)

	rows := h.mustHTTP("read_rows", map[string]any{"namespace": "fts", "table": "notes", "ids": []any{1}})
	list, _ := rows["rows"].([]any)
	row, _ := list[0].(map[string]any)
	if _, ok := row["_score"]; ok {
		t.Fatalf("read_rows must not carry a relevance score: %v", row)
	}
	data := h.mustHTTP("query", map[string]any{"namespace": "fts", "sql": "SELECT * FROM notes"})
	qrows, _ := data["rows"].([]any)
	first, _ := qrows[0].(map[string]any)
	if _, ok := first["_score"]; ok {
		t.Fatalf("query must not carry a relevance score: %v", first)
	}
}
