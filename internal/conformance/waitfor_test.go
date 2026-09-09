package conformance

import (
	"net/http"
	"testing"
	"time"
)

// Slice 5d conformance (§8.3 item 7's wait_for subset, §9.2 layer 2):
// wake-on-write from a second client, timeout-empty, timeout_ms 0's
// conditional poll, and the cursor-resume chain — through the transports, in
// auth:off like every other realtime case (§9.4).

// TestWaitForWakeOnWrite: a blocked waiter is woken by a matching commit
// from a second client and returns exactly that commit, while a commit on a
// different table never wakes a table-filtered waiter (§9.2 layer 2, §9.3:
// the wait selects the same feed changes_since reads).
func TestWaitForWakeOnWrite(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.seedTable("rt", "tasks", []map[string]any{{"name": "title", "type": "string"}})
	// The boundary is minted on the SAME feed the waiter polls — cursors
	// are per-feed, so a namespace-wide head token would be cross-feed
	// reuse, not a wait boundary.
	head := nextCursorOf(t, h.mustHTTP("wait_for",
		map[string]any{"namespace": "rt", "table": "notes", "timeout_ms": 0}))

	// The waiter blocks on the notes feed while other clients commit. A
	// generous timeout bounds a broken wait; the commits land within a
	// couple of ticks, so a working wake returns long before it.
	type waitOutcome struct {
		status int
		out    map[string]any
	}
	woken := make(chan waitOutcome, 1)
	go func() {
		// httpCall, not mustHTTP: FailNow from a non-test goroutine is not
		// allowed; the assertions happen on the receiving side.
		status, out := h.httpCall("wait_for", map[string]any{
			"namespace": "rt", "table": "notes", "cursor": head, "timeout_ms": 8000,
		})
		woken <- waitOutcome{status, out}
	}()
	// Settle past the waiter's first (empty) poll so the wake path — not
	// the first-read return — is what carries the test: two poll ticks.
	time.Sleep(600 * time.Millisecond)
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "tasks", "records": []any{map[string]any{"title": "foreign"}},
	})
	ins := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "wake"}},
	})

	select {
	case res := <-woken:
		if res.status != http.StatusOK || res.out["ok"] != true {
			t.Fatalf("wait_for woke with status %d %v", res.status, res.out)
		}
		data, _ := res.out["data"].(map[string]any)
		// Exactly the matching commit — the foreign table's event never
		// wakes the filtered waiter, and the matching commit that landed
		// mid-wait is never missed.
		want := [][3]any{{"notes", ins["ids"].([]any)[0], "insert"}}
		if got := changesOf(t, data); !changesEqual(got, want) {
			t.Fatalf("wake page = %v, want exactly the matching notes insert %v", got, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("wait_for never woke on a commit from a second client")
	}

	// The MCP surface wakes too: a fresh head start, then a commit lands
	// and the next wait over tools/call delivers it.
	mcpHead := nextCursorOf(t, h.mustMCP("wait_for", map[string]any{"namespace": "rt", "timeout_ms": 0}))
	mcpIns := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "mcp-wake"}},
	})
	sc := h.mustMCP("wait_for", map[string]any{"namespace": "rt", "cursor": mcpHead, "timeout_ms": 0})
	want := [][3]any{{"notes", mcpIns["ids"].([]any)[0], "insert"}}
	if got := changesOf(t, sc); !changesEqual(got, want) {
		t.Fatalf("MCP wake page = %v, want exactly the new notes commit %v", got, want)
	}
}

// TestWaitForTimeoutEmpty: a wait that outlives its bound returns an EMPTY
// page carrying the unchanged boundary cursor — never an error — and that
// cursor still resumes gap-free for commits that land after the timeout
// (§9.2: "empty result — never an error — on timeout").
func TestWaitForTimeoutEmpty(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "a"}},
	})
	head := nextCursorOf(t, h.mustHTTP("changes_since", map[string]any{"namespace": "rt"}))

	start := time.Now()
	data := h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "cursor": head, "timeout_ms": 400})
	held := time.Since(start)
	if got := changesOf(t, data); len(got) != 0 {
		t.Fatalf("timed-out wait returned %d changes, want an empty page", len(got))
	}
	cursor := nextCursorOf(t, data)
	// The wait held its bound: with nothing to deliver it cannot return
	// before the deadline, and a dropped timeout_ms would have held the
	// 30s default.
	if held < 380*time.Millisecond {
		t.Fatalf("timed-out wait returned after %v, want it to hold the 400ms bound", held)
	}
	if held > 5*time.Second {
		t.Fatalf("timed-out wait held %v — far past timeout_ms 400", held)
	}

	// The unchanged cursor resumes gap-free for what committed meanwhile.
	ins := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "later"}},
	})
	data = h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "cursor": cursor, "timeout_ms": 0})
	want := [][3]any{{"notes", ins["ids"].([]any)[0], "insert"}}
	if got := changesOf(t, data); !changesEqual(got, want) {
		t.Fatalf("resume after timeout = %v, want exactly the missed commit %v", got, want)
	}
}

// TestWaitForZeroTimeout: timeout_ms 0 is an immediate conditional poll —
// it returns the backlog a cursor already has, and an empty page at the
// head, without waiting (§9.2: "0 returns immediately").
func TestWaitForZeroTimeout(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}},
	})

	// A backlog-holding cursor returns its page at once.
	start := time.Now()
	data := h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "cursor": "begin", "timeout_ms": 0})
	if got := changesOf(t, data); len(got) != 2 {
		t.Fatalf("conditional poll over a backlog = %d changes, want 2", len(got))
	}
	if held := time.Since(start); held > 2*time.Second {
		t.Fatalf("timeout_ms 0 with a backlog held %v — it must return immediately", held)
	}

	// At the head there is nothing to return: the empty page and its
	// cursor, still immediate.
	start = time.Now()
	data = h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "timeout_ms": 0})
	if got := changesOf(t, data); len(got) != 0 {
		t.Fatalf("conditional poll at the head = %d changes, want an empty page", len(got))
	}
	nextCursorOf(t, data)
	if held := time.Since(start); held > 2*time.Second {
		t.Fatalf("timeout_ms 0 at the head held %v — it must return immediately", held)
	}
}

// TestWaitForCursorResumeChain: waits chained through next_cursor deliver
// every commit exactly once, in commit order — the loop a sleeping agent
// runs, including the quiet-timeout-then-resume step (§9.3's cursor
// durability through the long-poll).
func TestWaitForCursorResumeChain(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})

	// First call pins the head; subsequent commits are delivered by waits
	// resuming from each page's next_cursor.
	cursor := nextCursorOf(t, h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "timeout_ms": 0}))

	var want, got [][3]any
	for i := 0; i < 3; i++ {
		ins := h.mustHTTP("insert", map[string]any{
			"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "chain"}},
		})
		want = append(want, [3]any{"notes", ins["ids"].([]any)[0], "insert"})
		data := h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "cursor": cursor, "timeout_ms": 2000})
		page := changesOf(t, data)
		if len(page) != 1 {
			t.Fatalf("chain wait %d delivered %d changes, want exactly its own commit", i, len(page))
		}
		got = append(got, page...)
		cursor = nextCursorOf(t, data)
	}
	if !changesEqual(got, want) {
		t.Fatalf("wait chain = %v, want each commit exactly once in order %v", got, want)
	}

	// A quiet wait times out empty and keeps the chain intact for the next
	// commit — the empty page's cursor is the same boundary.
	data := h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "cursor": cursor, "timeout_ms": 300})
	if page := changesOf(t, data); len(page) != 0 {
		t.Fatalf("quiet chain wait delivered %v, want an empty timeout page", page)
	}
	cursor = nextCursorOf(t, data)
	after := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "after"}},
	})
	data = h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "cursor": cursor, "timeout_ms": 0})
	want = [][3]any{{"notes", after["ids"].([]any)[0], "insert"}}
	if page := changesOf(t, data); !changesEqual(page, want) {
		t.Fatalf("chain resume after a timeout = %v, want %v", page, want)
	}
}

// TestWaitForIdleLoopMintsNothing: the documented wait loop's intermediate
// polls mint nothing durable — an empty page re-presents the caller's own
// cursor (an unchanged position is not an issuance) — so a sleeping agent's
// waits never grow the cursor-token table. A wait long enough to run
// several poll ticks leaves the row count exactly where it was.
func TestWaitForIdleLoopMintsNothing(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	countTokens := func() int {
		var c int
		h.outOfBand("rt", func(db *sqlDB) error {
			return db.QueryRow(`SELECT count(*) FROM _dolmen_cursor_tokens`).Scan(&c)
		})
		return c
	}

	head := nextCursorOf(t, h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "timeout_ms": 0}))
	before := countTokens()
	data := h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "cursor": head, "timeout_ms": 700})
	if got := changesOf(t, data); len(got) != 0 {
		t.Fatalf("quiet wait delivered %d changes, want an empty page", len(got))
	}
	if next := nextCursorOf(t, data); next != head {
		t.Fatalf("idle wait swapped cursors %q → %q — an empty page must re-present the caller's own", head, next)
	}
	if after := countTokens(); after != before {
		t.Fatalf("idle wait grew the token table %d → %d — intermediate polls must mint nothing", before, after)
	}
}

// TestWaitForNeverCreatesNamespace: a wait never creates its namespace —
// §6.2's engine rule (never create implicitly) held at the op layer, where
// a data op's create-on-first-use would instead turn a typo'd name into a
// silent forever-empty wait, and an abandoned wait could even resurrect a
// namespace a concurrent drop just deleted. A missing namespace is
// not_found on both transports, and it stays missing.
func TestWaitForNeverCreatesNamespace(t *testing.T) {
	h := newHarness(t)
	status, out := h.httpCall("wait_for", map[string]any{"namespace": "ghost", "timeout_ms": 0})
	if status != http.StatusNotFound {
		t.Fatalf("missing-namespace wait status = %d %v, want 404", status, out)
	}
	if errEnv, _ := out["error"].(map[string]any); errEnv["code"] != "not_found" {
		t.Fatalf("missing-namespace wait code = %v, want not_found", errEnv["code"])
	}
	res := h.mcpCall("wait_for", map[string]any{"namespace": "ghost", "timeout_ms": 0})
	if !res.isError() {
		t.Fatalf("MCP missing-namespace wait must be a tool error, got %+v", res)
	}
	if env := res.toolError(); env["code"] != "not_found" {
		t.Fatalf("MCP missing-namespace wait code = %v, want not_found", env["code"])
	}
	nss, _ := h.mustHTTP("list_namespaces", map[string]any{})["namespaces"].([]any)
	if len(nss) != 0 {
		t.Fatalf("wait_for created namespaces: %v", nss)
	}
}

// TestWaitForTimeoutContract: timeout_ms valid 0–60000 inclusive — outside,
// wrong-typed, or null is invalid_request on both transports, and the shared
// feed selectors (limit bounds, non-empty cursor) validate through wait_for's
// surface too.
func TestWaitForTimeoutContract(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "a"}},
	})
	// The valid-range calls below resume from "begin" so they always have a
	// backlog to return — an at-head wait would hold its full timeout.

	for _, bad := range []any{-1, 60001, 300000, 0.5, "500", true, nil} {
		status, out := h.httpCall("wait_for", map[string]any{"namespace": "rt", "cursor": "begin", "timeout_ms": bad})
		if status != http.StatusBadRequest {
			t.Fatalf("timeout_ms %v (%T): status = %d %v, want 400", bad, bad, status, out)
		}
		if errEnv, _ := out["error"].(map[string]any); errEnv["code"] != "invalid_request" {
			t.Fatalf("timeout_ms %v: error code = %v, want invalid_request", bad, errEnv["code"])
		}
	}
	for _, bad := range []any{0, -1, 1001, "50", nil} {
		status, _ := h.httpCall("wait_for", map[string]any{"namespace": "rt", "cursor": "begin", "limit": bad, "timeout_ms": 0})
		if status != http.StatusBadRequest {
			t.Fatalf("limit %v (%T): status = %d, want 400", bad, bad, status)
		}
	}
	for _, empty := range []any{"", "   "} {
		status, _ := h.httpCall("wait_for", map[string]any{"namespace": "rt", "cursor": empty, "timeout_ms": 0})
		if status != http.StatusBadRequest {
			t.Fatalf("cursor %q: status = %d, want 400", empty, status)
		}
	}

	// Both bounds are inclusive; the MCP surface rejects out-of-range with
	// the same teaching shape.
	for _, ok := range []any{0, 1, 30000, 60000} {
		if status, _ := h.httpCall("wait_for", map[string]any{"namespace": "rt", "cursor": "begin", "timeout_ms": ok}); status != http.StatusOK {
			t.Fatalf("timeout_ms %v rejected — the range is inclusive", ok)
		}
	}
	res := h.mcpCall("wait_for", map[string]any{"namespace": "rt", "timeout_ms": 60001})
	if !res.isError() {
		t.Fatalf("MCP out-of-range timeout must be a tool error, got %+v", res)
	}
	if env := res.toolError(); env["code"] != "invalid_request" {
		t.Fatalf("MCP out-of-range timeout code = %v, want invalid_request", env["code"])
	}
}
