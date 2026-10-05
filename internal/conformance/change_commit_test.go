package conformance

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

func changeCommit(t *testing.T, change map[string]any) float64 {
	t.Helper()
	commit, ok := change["commit"].(float64)
	if !ok || commit <= 0 || commit != float64(int64(commit)) {
		t.Fatalf("change has no positive integer commit: %v", change)
	}
	return commit
}

func TestChangeCommitsSurvivePaginationReplayAndTransports(t *testing.T) {
	h := newHarness(t)
	h.seedTable("commits", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("create_table", map[string]any{"namespace": "commits", "table": "other", "fields": []map[string]any{{"name": "title", "type": "string"}}})
	body := map[string]any{"namespace": "commits", "idempotency_key": "once", "writes": []map[string]any{
		{"kind": "insert", "table": "notes", "records": []map[string]any{{"title": "one"}, {"title": "two"}}},
		{"kind": "insert", "table": "other", "records": []map[string]any{{"title": "three"}}},
		{"kind": "update", "table": "notes", "filter": "title = 'one'", "set": map[string]any{"title": "updated"}},
		{"kind": "delete", "table": "notes", "filter": "title = 'two'"},
	}}
	h.mustHTTP("batch", body)
	replay := h.mustHTTP("batch", body)
	if replay["replayed"] != true {
		t.Fatalf("batch was not replayed: %v", replay)
	}
	h.mustHTTP("insert", map[string]any{"namespace": "commits", "table": "notes", "records": []map[string]any{{"title": "later"}}})
	cursor := "begin"
	var first, second float64
	for i := 0; i < 6; i++ {
		page := h.mustHTTP("changes_since", map[string]any{"namespace": "commits", "cursor": cursor, "limit": 1})
		changes := page["changes"].([]any)
		if len(changes) != 1 {
			t.Fatalf("page %d: %v", i, page)
		}
		commit := changeCommit(t, changes[0].(map[string]any))
		if i == 0 {
			first = commit
		}
		if i < 5 && commit != first {
			t.Fatalf("batch split into different commits: %v != %v", commit, first)
		}
		if i == 5 {
			second = commit
			if second <= first {
				t.Fatalf("later commit %v <= %v", second, first)
			}
		}
		cursor = nextCursorOf(t, page)
	}
	if got := h.mustHTTP("changes_since", map[string]any{"namespace": "commits", "cursor": cursor})["changes"].([]any); len(got) != 0 {
		t.Fatalf("replay minted more changes: %v", got)
	}
	for _, transport := range []string{"http", "mcp"} {
		for _, op := range []string{"changes_since", "wait_for"} {
			args := map[string]any{"namespace": "commits", "cursor": "begin"}
			if op == "wait_for" {
				args["timeout_ms"] = 0
			}
			var page map[string]any
			if transport == "http" {
				page = h.mustHTTP(op, args)
			} else {
				page = h.mustMCP(op, args)
			}
			changes := page["changes"].([]any)
			if len(changes) != 6 {
				t.Fatalf("%s %s: %v", transport, op, page)
			}
			for i, raw := range changes {
				want := first
				if i == 5 {
					want = second
				}
				if got := changeCommit(t, raw.(map[string]any)); got != want {
					t.Fatalf("re-read changed commit %v to %v", want, got)
				}
			}
		}
	}
	table := h.mustHTTP("changes_since", map[string]any{"namespace": "commits", "table": "other", "cursor": "begin"})["changes"].([]any)
	if len(table) != 1 || changeCommit(t, table[0].(map[string]any)) != first {
		t.Fatalf("table feed lost batch identity: %v", table)
	}
	r := h.subscribeStream(t, url.Values{"namespace": {"commits"}, "cursor": {"begin"}})
	for i := 0; i < 6; {
		frame, ok := r.next(10 * time.Second)
		if !ok {
			t.Fatalf("missing SSE change %d", i)
		}
		if frame.event != "change" {
			continue
		}
		var change map[string]any
		if err := json.Unmarshal([]byte(frame.data), &change); err != nil {
			t.Fatal(err)
		}
		want := first
		if i == 5 {
			want = second
		}
		if got := changeCommit(t, change); got != want {
			t.Fatalf("SSE commit %v, want %v", got, want)
		}
		i++
	}
}

func TestRolledBackBatchMintsNoCommitRecords(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rollback", "notes", []map[string]any{{"name": "title", "type": "string"}})
	batchError(t, h, `{"namespace":"rollback","writes":[{"kind":"insert","table":"notes","records":[{"title":"undone"}]},{"kind":"insert","table":"notes","records":[{"unknown":"fail"}]}]}`)
	h.mustHTTP("insert", map[string]any{"namespace": "rollback", "table": "notes", "records": []map[string]any{{"title": "kept"}}})
	changes := h.mustHTTP("changes_since", map[string]any{"namespace": "rollback", "cursor": "begin"})["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("rollback leaked events: %v", changes)
	}
	changeCommit(t, changes[0].(map[string]any))
}
