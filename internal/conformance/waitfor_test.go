package conformance

import (
	"net/http"
	"testing"
	"time"
)

func TestWaitForWakeOnWrite(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.seedTable("rt", "tasks", []map[string]any{{"name": "title", "type": "string"}})

	head := nextCursorOf(t, h.mustHTTP("wait_for",
		map[string]any{"namespace": "rt", "table": "notes", "timeout_ms": 0}))

	type waitOutcome struct {
		status int
		out    map[string]any
	}
	woken := make(chan waitOutcome, 1)
	go func() {

		status, out := h.httpCall("wait_for", map[string]any{
			"namespace": "rt", "table": "notes", "cursor": head, "timeout_ms": 8000,
		})
		woken <- waitOutcome{status, out}
	}()

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

		want := [][3]any{{"notes", ins["ids"].([]any)[0], "insert"}}
		if got := changesOf(t, data); !changesEqual(got, want) {
			t.Fatalf("wake page = %v, want exactly the matching notes insert %v", got, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("wait_for never woke on a commit from a second client")
	}

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

	if held < 380*time.Millisecond {
		t.Fatalf("timed-out wait returned after %v, want it to hold the 400ms bound", held)
	}
	if held > 5*time.Second {
		t.Fatalf("timed-out wait held %v — far past timeout_ms 400", held)
	}

	ins := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "later"}},
	})
	data = h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "cursor": cursor, "timeout_ms": 0})
	want := [][3]any{{"notes", ins["ids"].([]any)[0], "insert"}}
	if got := changesOf(t, data); !changesEqual(got, want) {
		t.Fatalf("resume after timeout = %v, want exactly the missed commit %v", got, want)
	}
}

func TestWaitForZeroTimeout(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}},
	})

	start := time.Now()
	data := h.mustHTTP("wait_for", map[string]any{"namespace": "rt", "cursor": "begin", "timeout_ms": 0})
	if got := changesOf(t, data); len(got) != 2 {
		t.Fatalf("conditional poll over a backlog = %d changes, want 2", len(got))
	}
	if held := time.Since(start); held > 2*time.Second {
		t.Fatalf("timeout_ms 0 with a backlog held %v — it must return immediately", held)
	}

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

func TestWaitForCursorResumeChain(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})

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

func TestWaitForIdleLoopMintsNothing(t *testing.T) {
	sqliteOnly(t)
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

func TestWaitForTimeoutContract(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "a"}},
	})

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
