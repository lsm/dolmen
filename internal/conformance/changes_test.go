package conformance

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Slice 5c conformance (§8.3 item 7's changes_since subset, §9.2–9.3):
// event-on-write, cursor replay after reopen, gap-free sequences, reconnect
// catch-up, and the beyond-retention teaching error — all through the
// transports, in auth:off like every other realtime case (§9.4).

// changesOf projects a changes_since data object as comparable
// (table, row_id, kind) triples.
func changesOf(t *testing.T, data map[string]any) [][3]any {
	t.Helper()
	raw, ok := data["changes"].([]any)
	if !ok {
		t.Fatalf("changes_since returned no changes array: %v", data)
	}
	out := make([][3]any, 0, len(raw))
	for i, c := range raw {
		m, ok := c.(map[string]any)
		if !ok {
			t.Fatalf("change %d is not an object: %v", i, c)
		}
		for _, k := range []string{"cursor", "table", "row_id", "kind"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("change %d is missing %q: %v", i, k, m)
			}
		}
		if len(m) != 4 {
			t.Fatalf("change %d carries fields beyond cursor/table/row_id/kind: %v", i, m)
		}
		out = append(out, [3]any{m["table"], m["row_id"], m["kind"]})
	}
	return out
}

// nextCursorOf extracts a page's next_cursor, which must always be present.
func nextCursorOf(t *testing.T, data map[string]any) string {
	t.Helper()
	next, ok := data["next_cursor"].(string)
	if !ok || next == "" {
		t.Fatalf("changes_since returned no next_cursor: %v", data)
	}
	return next
}

// changesEqual compares two projected pages.
func changesEqual(a, b [][3]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestChangesSinceEventOnWrite: a commit produces its change records in
// commit order — insert, update, and delete each leave exactly their own
// event, addressed by table and row id (§9.2 layer 1, §9.3).
func TestChangesSinceEventOnWrite(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})

	// A fresh subscriber starts at the head: the backlog stays unreplayed and
	// the response carries the head cursor (§9.3's bare-start semantics).
	data := h.mustHTTP("changes_since", map[string]any{"namespace": "rt"})
	if got := changesOf(t, data); len(got) != 0 {
		t.Fatalf("bare start replayed %d changes, want 0 (head start, no backlog)", len(got))
	}
	head := nextCursorOf(t, data)

	ins := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}},
	})
	ids := ins["ids"].([]any)
	h.mustHTTP("update", map[string]any{
		"namespace": "rt", "table": "notes", "filter": "id = ?",
		"args": []any{ids[0]}, "set": map[string]any{"title": "a2"},
	})
	h.mustHTTP("delete", map[string]any{
		"namespace": "rt", "table": "notes", "filter": "id = ?", "args": []any{ids[0]}, "confirm": true,
	})

	data = h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": head})
	want := [][3]any{
		{"notes", ids[0], "insert"},
		{"notes", ids[1], "insert"},
		{"notes", ids[0], "update"},
		{"notes", ids[0], "delete"},
	}
	if got := changesOf(t, data); !changesEqual(got, want) {
		t.Fatalf("event-on-write = %v, want %v", got, want)
	}
}

// TestChangesSinceCursorReplayAfterReopen: a restarted client replays from
// its stored cursor, gap-free — the token mapping is durable in the namespace
// db, so a restart never invalidates it (§9.3).
func TestChangesSinceCursorReplayAfterReopen(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}, map[string]any{"title": "c"}},
	})
	data := h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": "begin", "limit": 2})
	if got := changesOf(t, data); len(got) != 2 {
		t.Fatalf("first page = %d changes, want 2 (limit)", len(got))
	}
	cursor := nextCursorOf(t, data)

	h.reopen()

	// A write that lands while the client is down.
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "d"}},
	})
	data = h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": cursor})
	want := [][3]any{{"notes", float64(3), "insert"}, {"notes", float64(4), "insert"}}
	if got := changesOf(t, data); !changesEqual(got, want) {
		t.Fatalf("replay after reopen = %v, want %v — the pre-restart page boundary plus the mid-down write", got, want)
	}
}

// TestChangesSinceGapFreeSequences: paging a backlog with limit below the
// log delivers every change exactly once in commit order — the concatenation
// of pages is the whole feed with no gaps and no duplicates, across tables
// (§9.3).
func TestChangesSinceGapFreeSequences(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.seedTable("rt", "tasks", []map[string]any{{"name": "title", "type": "string"}})

	var want [][3]any
	add := func(table string, n int) {
		recs := make([]any, n)
		for i := range recs {
			recs[i] = map[string]any{"title": "x"}
		}
		res := h.mustHTTP("insert", map[string]any{
			"namespace": "rt", "table": table, "records": recs,
		})
		for _, id := range res["ids"].([]any) {
			want = append(want, [3]any{table, id, "insert"})
		}
	}
	add("notes", 3)
	add("tasks", 2)
	add("notes", 2)

	var got [][3]any
	cursor := "begin"
	for {
		data := h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": cursor, "limit": 3})
		page := changesOf(t, data)
		got = append(got, page...)
		if len(page) < 3 {
			break
		}
		cursor = nextCursorOf(t, data)
	}
	if !changesEqual(got, want) {
		t.Fatalf("paged feed is not the gap-free commit order:\ngot:  %v\nwant: %v", got, want)
	}
}

// TestChangesSinceReconnectCatchUp: a disconnected client's resume equals
// exactly the events it missed — the reconnect recipe is changes_since
// catch-up (§8.3 item 7).
func TestChangesSinceReconnectCatchUp(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "seen"}},
	})
	// The "connection" ends holding this cursor.
	cursor := nextCursorOf(t, h.mustHTTP("changes_since", map[string]any{"namespace": "rt"}))

	missed := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "m1"}, map[string]any{"title": "m2"}},
	})
	missedIDs := missed["ids"].([]any)

	data := h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": cursor})
	want := [][3]any{
		{"notes", missedIDs[0], "insert"},
		{"notes", missedIDs[1], "insert"},
	}
	if got := changesOf(t, data); !changesEqual(got, want) {
		t.Fatalf("reconnect catch-up = %v, want exactly the missed events %v", got, want)
	}

	// And the stream continues from there without repeating them.
	after := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "after"}},
	})
	data = h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": nextCursorOf(t, data)})
	want = [][3]any{{"notes", after["ids"].([]any)[0], "insert"}}
	if got := changesOf(t, data); !changesEqual(got, want) {
		t.Fatalf("post-reconnect resume = %v, want only the new event %v", got, want)
	}
}

// TestChangesSinceBeyondRetentionTeachingError: a cursor past the retention
// window is an explicit teaching error naming the catch-up path — never a
// silent empty page or short read (§9.3) — on both transports.
func TestChangesSinceBeyondRetentionTeachingError(t *testing.T) {
	h := newHarnessRetention(t, 40*time.Millisecond)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "a"}},
	})
	cursor := nextCursorOf(t, h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": "begin"}))
	time.Sleep(250 * time.Millisecond)

	status, out := h.httpCall("changes_since", map[string]any{"namespace": "rt", "cursor": cursor})
	if status != http.StatusBadRequest {
		t.Fatalf("beyond-retention status = %d %v, want 400", status, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if code, _ := errEnv["code"].(string); code != "invalid_request" {
		t.Fatalf("beyond-retention code = %q, want invalid_request", code)
	}
	msg, _ := errEnv["message"].(string)
	for _, teach := range []string{"changes_since", "begin"} {
		if !strings.Contains(msg, teach) {
			t.Fatalf("beyond-retention message %q does not name the catch-up path (%q missing)", msg, teach)
		}
	}

	// The MCP surface carries the same teaching error, not a protocol one.
	res := h.mcpCall("changes_since", map[string]any{"namespace": "rt", "cursor": cursor})
	if !res.isError() {
		t.Fatalf("MCP beyond-retention call must be a tool error, got %+v", res)
	}

	// The catch-up paths work: a fresh head start and a begin replay.
	h.mustHTTP("changes_since", map[string]any{"namespace": "rt"})
	h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": "begin"})
}

// TestChangesSinceLimitContract: limit default 100, max 1000 — an explicit
// value outside 1–1000 (or of the wrong type) is invalid_request (§9.3).
func TestChangesSinceLimitContract(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	// 105 changes: the default page is 100, not everything.
	for i := 0; i < 26; i++ {
		h.mustHTTP("insert", map[string]any{
			"namespace": "rt", "table": "notes",
			"records": []any{
				map[string]any{"title": "a"}, map[string]any{"title": "b"},
				map[string]any{"title": "c"}, map[string]any{"title": "d"},
			},
		})
	}
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "e"}},
	})

	data := h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": "begin"})
	if got := changesOf(t, data); len(got) != 100 {
		t.Fatalf("default limit page = %d changes, want 100", len(got))
	}

	for _, bad := range []any{0, -1, 1001, "50", 2.5, nil} {
		status, out := h.httpCall("changes_since", map[string]any{"namespace": "rt", "cursor": "begin", "limit": bad})
		if status != http.StatusBadRequest {
			t.Fatalf("limit %v (%T): status = %d %v, want 400", bad, bad, status, out)
		}
		if errEnv, _ := out["error"].(map[string]any); errEnv["code"] != "invalid_request" {
			t.Fatalf("limit %v: error code = %v, want invalid_request", bad, errEnv["code"])
		}
	}

	// 1000 — the ceiling — is valid and clamps nothing below it.
	if status, _ := h.httpCall("changes_since", map[string]any{"namespace": "rt", "cursor": "begin", "limit": 1000}); status != http.StatusOK {
		t.Fatalf("limit 1000 rejected — the maximum is inclusive")
	}
}

// TestChangesSinceTableFeedContract: the optional table filter selects only
// that table's CURRENT lifetime (a dropped-and-recreated successor's feed
// never replays the predecessor's records), a missing table's feed is
// not_found, and a cursor is bound to its feed (§9.3).
func TestChangesSinceTableFeedContract(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.seedTable("rt", "tasks", []map[string]any{{"name": "title", "type": "string"}})
	first := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "a"}},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "tasks", "records": []any{map[string]any{"title": "t"}},
	})

	// The table feed sees only its table.
	data := h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "table": "notes", "cursor": "begin"})
	want := [][3]any{{"notes", first["ids"].([]any)[0], "insert"}}
	if got := changesOf(t, data); !changesEqual(got, want) {
		t.Fatalf("table feed = %v, want only notes events %v", got, want)
	}
	tableCursor := nextCursorOf(t, data)

	// A feed for a table that does not exist is not_found.
	status, out := h.httpCall("changes_since", map[string]any{"namespace": "rt", "table": "missing"})
	if status != http.StatusNotFound {
		t.Fatalf("missing table feed status = %d %v, want 404", status, out)
	}
	if errEnv, _ := out["error"].(map[string]any); errEnv["code"] != "not_found" {
		t.Fatalf("missing table feed code = %v, want not_found", errEnv["code"])
	}

	// Cross-feed reuse is rejected with the teaching shape, never honored.
	status, out = h.httpCall("changes_since", map[string]any{"namespace": "rt", "cursor": tableCursor})
	if status != http.StatusBadRequest {
		t.Fatalf("cross-feed status = %d %v, want 400", status, out)
	}
	if errEnv, _ := out["error"].(map[string]any); errEnv["code"] != "invalid_request" {
		t.Fatalf("cross-feed code = %v, want invalid_request", errEnv["code"])
	}

	// Drop and recreate: begin on the successor's feed replays only the
	// successor's lifetime, and a cursor from before the drop never surfaces
	// the predecessor's records either.
	h.mustHTTP("drop_table", map[string]any{"namespace": "rt", "table": "notes", "confirm": "notes"})
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	next := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "b"}},
	})
	data = h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "table": "notes", "cursor": "begin"})
	want = [][3]any{{"notes", next["ids"].([]any)[0], "insert"}}
	if got := changesOf(t, data); !changesEqual(got, want) {
		t.Fatalf("successor table feed = %v, want only the successor's event %v", got, want)
	}
	data = h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "table": "notes", "cursor": tableCursor})
	if got := changesOf(t, data); !changesEqual(got, want) {
		t.Fatalf("pre-drop cursor on the successor feed = %v, want only the successor's event %v", got, want)
	}
}

// TestChangesSinceTransportParity: the same changes_since call over /v1 and
// tools/call returns the same page (§2's one-contract rule).
func TestChangesSinceTransportParity(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	ins := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "a"}},
	})

	httpData := h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": "begin"})
	mcpData := h.mustMCP("changes_since", map[string]any{"namespace": "rt", "cursor": "begin"})

	// The projected change set must match exactly; cursors are fresh opaque
	// randomness per issuance (§9.3), so tokens themselves never compare.
	if got, want := changesOf(t, httpData), changesOf(t, mcpData); !changesEqual(got, want) {
		t.Fatalf("transport parity broke:\nhttp: %v\nmcp:  %v", got, want)
	}
	if got := changesOf(t, httpData); len(got) != 1 || got[0] != [3]any{"notes", ins["ids"].([]any)[0], "insert"} {
		t.Fatalf("parity page = %v, want the one insert", got)
	}
	if nextCursorOf(t, mcpData) == "" {
		t.Fatalf("MCP page carried no next_cursor")
	}
}
