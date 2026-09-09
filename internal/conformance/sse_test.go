package conformance

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
)

// Slice 6a conformance (§9.2 layer 3, replay half): the subscribe SSE handler
// — replay in cursor order, then a terminal close frame; teaching errors as
// SSE error events carrying the standard envelope. The route itself stays
// UNREGISTERED until 6b (a discovered replay-then-terminate route would read
// as the endpoint's specified live-stream behavior), so every stream here is
// handler-direct: httptest against the handler, read as a stream with bufio.

// sseFrame is one parsed server-sent event: its named event type and data
// line. Comment lines and retry hints carry neither and are dropped.
type sseFrame struct {
	event string
	data  string
}

// readSSEFrames streams every frame off an already-open response body until
// the handler ends the stream, parsing the wire framing line by line — the
// read a live EventSource performs, not a buffered slurp.
func readSSEFrames(t *testing.T, res *http.Response) []sseFrame {
	t.Helper()
	defer res.Body.Close()
	var frames []sseFrame
	var cur sseFrame
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if cur.event != "" || cur.data != "" {
				frames = append(frames, cur)
				cur = sseFrame{}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read subscribe stream: %v", err)
	}
	return frames
}

// frameData decodes a frame's data line as a JSON object.
func frameData(t *testing.T, f sseFrame) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(f.data), &m); err != nil {
		t.Fatalf("frame %q data is not a JSON object: %v (%q)", f.event, err, f.data)
	}
	return m
}

// wantFrame fails unless frames[i] is an event of the named type.
func wantFrame(t *testing.T, frames []sseFrame, i int, event string) sseFrame {
	t.Helper()
	if i >= len(frames) {
		t.Fatalf("expected frame %d (%s event), got only %d frames", i, event, len(frames))
	}
	if frames[i].event != event {
		t.Fatalf("frame %d = %q event, want %q (data %q)", i, frames[i].event, event, frames[i].data)
	}
	return frames[i]
}

// subscribe opens a handler-direct subscribe stream over the harness's
// CURRENT api server — the route is registered on no mux until 6b — and
// returns the response with its frames unread.
func (h *harness) subscribe(t *testing.T, query url.Values) *http.Response {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(h.api.HandleSubscribe))
	t.Cleanup(srv.Close)
	res, err := http.Get(srv.URL + "/v1/subscribe?" + query.Encode())
	if err != nil {
		t.Fatalf("open subscribe stream: %v", err)
	}
	return res
}

// subscribeFrames is subscribe plus a full read of the stream.
func (h *harness) subscribeFrames(t *testing.T, query url.Values) []sseFrame {
	t.Helper()
	return readSSEFrames(t, h.subscribe(t, query))
}

// sseChangeOf projects a change frame's data as a comparable
// (table, row_id, kind) triple — changesOf's SSE twin — and pins that the
// event carries the four-field public projection and nothing else.
func sseChangeOf(t *testing.T, f sseFrame) [3]any {
	t.Helper()
	if f.event != "change" {
		t.Fatalf("sseChangeOf on %q event, want change", f.event)
	}
	m := frameData(t, f)
	for _, k := range []string{"cursor", "table", "row_id", "kind"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("change event is missing %q: %v", k, m)
		}
	}
	if len(m) != 4 {
		t.Fatalf("change event carries fields beyond cursor/table/row_id/kind: %v", m)
	}
	if c, _ := m["cursor"].(string); c == "" {
		t.Fatalf("change event carries an empty cursor: %v", m)
	}
	return [3]any{m["table"], m["row_id"], m["kind"]}
}

// TestSubscribeReplayThenClose: the slice's core contract — a subscriber
// holding a cursor receives exactly the events committed after it, in commit
// order, then a terminal close frame carrying the boundary cursor, and the
// stream ends there (§9.2 layer 3, §9.3).
func TestSubscribeReplayThenClose(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "seen"}},
	})
	// The "connection" opens holding this cursor.
	head := nextCursorOf(t, h.mustHTTP("changes_since", map[string]any{"namespace": "rt"}))

	missed := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "m1"}, map[string]any{"title": "m2"}},
	})
	missedIDs := missed["ids"].([]any)

	res := h.subscribe(t, url.Values{"namespace": {"rt"}, "cursor": {head}})
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("subscribe content-type = %q, want text/event-stream", ct)
	}
	if res.Header.Get("X-Request-Id") == "" {
		t.Fatalf("subscribe stream carries no X-Request-Id")
	}
	frames := readSSEFrames(t, res)

	if len(frames) != 3 {
		t.Fatalf("replay stream = %d frames, want 2 change events + close: %+v", len(frames), frames)
	}
	want := [][3]any{
		{"notes", missedIDs[0], "insert"},
		{"notes", missedIDs[1], "insert"},
	}
	for i, w := range want {
		if got := sseChangeOf(t, wantFrame(t, frames, i, "change")); got != w {
			t.Fatalf("change %d = %v, want %v", i, got, w)
		}
	}
	closeData := frameData(t, wantFrame(t, frames, 2, "close"))
	closeCursor, _ := closeData["cursor"].(string)
	if closeCursor == "" {
		t.Fatalf("close frame carries no cursor: %v", closeData)
	}
	if len(closeData) != 1 {
		t.Fatalf("close frame carries fields beyond cursor: %v", closeData)
	}

	// Resuming from the close cursor delivers nothing again — the boundary
	// held, no duplicates, and a fresh write after it arrives exactly once.
	after := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "after"}},
	})
	frames = h.subscribeFrames(t, url.Values{"namespace": {"rt"}, "cursor": {closeCursor}})
	if len(frames) != 2 {
		t.Fatalf("post-close resume = %d frames, want 1 change event + close: %+v", len(frames), frames)
	}
	if got, w := sseChangeOf(t, wantFrame(t, frames, 0, "change")), [3]any{"notes", after["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("post-close resume = %v, want only the new event %v", got, w)
	}
	wantFrame(t, frames, 1, "close")
}

// TestSubscribeCursorForms: the two pinned cursor forms beside a resume
// token — omitted starts at the current head (wake-up semantics: nothing
// replays, the close frame carries the head cursor, only subsequent commits
// are delivered), and "begin" replays retained history from the oldest
// readable boundary (§9.3).
func TestSubscribeCursorForms(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	first := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}},
	})
	firstIDs := first["ids"].([]any)

	// Omitted cursor: a fresh subscriber gets future events only.
	frames := h.subscribeFrames(t, url.Values{"namespace": {"rt"}})
	if len(frames) != 1 {
		t.Fatalf("bare start = %d frames, want close only: %+v", len(frames), frames)
	}
	head, _ := frameData(t, wantFrame(t, frames, 0, "close"))["cursor"].(string)
	if head == "" {
		t.Fatalf("bare-start close frame carries no head cursor: %+v", frames)
	}

	// The next commit — and only it — is delivered from that head.
	next := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "c"}},
	})
	frames = h.subscribeFrames(t, url.Values{"namespace": {"rt"}, "cursor": {head}})
	if len(frames) != 2 {
		t.Fatalf("head resume = %d frames, want 1 change event + close: %+v", len(frames), frames)
	}
	if got, w := sseChangeOf(t, wantFrame(t, frames, 0, "change")), [3]any{"notes", next["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("head resume = %v, want only the new event %v", got, w)
	}

	// begin: the whole retained backlog, in commit order.
	frames = h.subscribeFrames(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	if len(frames) != 4 {
		t.Fatalf("begin replay = %d frames, want 3 change events + close: %+v", len(frames), frames)
	}
	want := [][3]any{
		{"notes", firstIDs[0], "insert"},
		{"notes", firstIDs[1], "insert"},
		{"notes", next["ids"].([]any)[0], "insert"},
	}
	for i, w := range want {
		if got := sseChangeOf(t, wantFrame(t, frames, i, "change")); got != w {
			t.Fatalf("begin change %d = %v, want %v", i, got, w)
		}
	}
	wantFrame(t, frames, 3, "close")
}

// TestSubscribeTableFilter: the optional table parameter selects that
// table's CURRENT lifetime only, a cursor minted on a table feed is bound to
// it, and a feed for a table that does not exist is the not_found teaching
// error as an in-stream error event (§9.3).
func TestSubscribeTableFilter(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.seedTable("rt", "tasks", []map[string]any{{"name": "title", "type": "string"}})
	notes := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "n"}},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "tasks", "records": []any{map[string]any{"title": "t"}},
	})

	// The table feed sees only its table's events.
	frames := h.subscribeFrames(t, url.Values{"namespace": {"rt"}, "table": {"notes"}, "cursor": {"begin"}})
	if len(frames) != 2 {
		t.Fatalf("table feed = %d frames, want 1 change event + close: %+v", len(frames), frames)
	}
	if got, w := sseChangeOf(t, wantFrame(t, frames, 0, "change")), [3]any{"notes", notes["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("table feed = %v, want only the notes event %v", got, w)
	}
	tableCursor, _ := frameData(t, wantFrame(t, frames, 1, "close"))["cursor"].(string)

	// A missing table's feed is not_found, in-stream.
	frames = h.subscribeFrames(t, url.Values{"namespace": {"rt"}, "table": {"missing"}})
	if len(frames) != 1 {
		t.Fatalf("missing table feed = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	errEnv := wantFrameError(t, frames, 0)
	if errEnv["code"] != "not_found" {
		t.Fatalf("missing table feed code = %v, want not_found", errEnv["code"])
	}

	// A table-feed cursor does not resolve on the namespace-wide feed.
	frames = h.subscribeFrames(t, url.Values{"namespace": {"rt"}, "cursor": {tableCursor}})
	if len(frames) != 1 {
		t.Fatalf("cross-feed = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	errEnv = wantFrameError(t, frames, 0)
	if errEnv["code"] != "invalid_request" {
		t.Fatalf("cross-feed code = %v, want invalid_request", errEnv["code"])
	}
	if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "different feed") {
		t.Fatalf("cross-feed message %q does not teach the feed binding", msg)
	}
}

// wantFrameError fails unless frames[i] is an error event and returns the
// standard error envelope it carries inside.
func wantFrameError(t *testing.T, frames []sseFrame, i int) map[string]any {
	t.Helper()
	f := wantFrame(t, frames, i, "error")
	env := frameData(t, f)
	if env["ok"] != false {
		t.Fatalf("error event data is not the standard envelope (ok:false): %v", env)
	}
	errEnv, _ := env["error"].(map[string]any)
	if errEnv == nil {
		t.Fatalf("error event carries no error object: %v", env)
	}
	if _, ok := errEnv["request_id"]; !ok {
		t.Fatalf("error event's envelope carries no request_id: %v", errEnv)
	}
	return errEnv
}

// TestSubscribeCursorTeachingErrors: a cursor that is unknown or past the
// change-log retention window gets the teaching error event naming the
// catch-up path — reconnecting with no cursor or with cursor=begin — never a
// silent empty replay, and the stream ends at the error event (§9.3).
func TestSubscribeCursorTeachingErrors(t *testing.T) {
	h := newHarnessRetention(t, 40*time.Millisecond)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "a"}},
	})
	cursor := nextCursorOf(t, h.mustHTTP("changes_since", map[string]any{"namespace": "rt", "cursor": "begin"}))
	time.Sleep(250 * time.Millisecond)

	// The expired token: teaching error, no close frame after it.
	frames := h.subscribeFrames(t, url.Values{"namespace": {"rt"}, "cursor": {cursor}})
	if len(frames) != 1 {
		t.Fatalf("beyond-retention = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	errEnv := wantFrameError(t, frames, 0)
	if errEnv["code"] != "invalid_request" {
		t.Fatalf("beyond-retention code = %v, want invalid_request", errEnv["code"])
	}
	msg, _ := errEnv["message"].(string)
	for _, teach := range []string{"retention", "no cursor", "begin"} {
		if !strings.Contains(msg, teach) {
			t.Fatalf("beyond-retention message %q does not name the catch-up path (%q missing)", msg, teach)
		}
	}

	// A token that never existed teaches the same path.
	frames = h.subscribeFrames(t, url.Values{"namespace": {"rt"}, "cursor": {"never-minted"}})
	if len(frames) != 1 {
		t.Fatalf("unknown cursor = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	if errEnv := wantFrameError(t, frames, 0); errEnv["code"] != "invalid_request" {
		t.Fatalf("unknown cursor code = %v, want invalid_request", errEnv["code"])
	}

	// Both catch-up paths work on the stream itself: a bare start closes at
	// the head with nothing replayed, and begin replays from the retained
	// boundary — a fresh commit made after the expiry, since the aged-out
	// record is beyond every replay guarantee now.
	frames = h.subscribeFrames(t, url.Values{"namespace": {"rt"}})
	if len(frames) != 1 || frames[0].event != "close" {
		t.Fatalf("post-error bare start = %+v, want close only", frames)
	}
	fresh := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "fresh"}},
	})
	frames = h.subscribeFrames(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	if len(frames) != 2 || frames[0].event != "change" || frames[1].event != "close" {
		t.Fatalf("post-error begin replay = %+v, want 1 change event + close", frames)
	}
	if got, w := sseChangeOf(t, frames[0]), [3]any{"notes", fresh["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("post-error begin replay = %v, want only the fresh event %v", got, w)
	}
}

// TestSubscribeShapeErrors: request-shape failures are answered before the
// stream opens — wrong method, missing namespace, and explicitly empty
// selectors get ordinary HTTP errors, mirroring the changes_since op's own
// empty-vs-omitted rules.
func TestSubscribeShapeErrors(t *testing.T) {
	h := newHarness(t)
	h.ensureNS("rt")

	for _, tt := range []struct {
		name  string
		query url.Values
	}{
		{"missing namespace", nil},
		{"empty namespace", url.Values{"namespace": {""}}},
		{"empty table", url.Values{"namespace": {"rt"}, "table": {""}}},
		{"blank table", url.Values{"namespace": {"rt"}, "table": {"   "}}},
		{"empty cursor", url.Values{"namespace": {"rt"}, "cursor": {""}}},
		{"blank cursor", url.Values{"namespace": {"rt"}, "cursor": {"   "}}},
	} {
		res := h.subscribe(t, tt.query)
		body := decodeBody(t, res)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (%v)", tt.name, res.StatusCode, body)
		}
		if errEnv, _ := body["error"].(map[string]any); errEnv["code"] != "invalid_request" {
			t.Fatalf("%s: error code = %v, want invalid_request", tt.name, errEnv["code"])
		}
	}

	// A namespace that is present but invalid ("   " normalizes to an empty
	// path) is the store's to reject — the op lets the same value reach
	// CreateNamespace and answer invalid_request, so the stream answers with
	// the in-stream error event, not a pre-stream status.
	frames := h.subscribeFrames(t, url.Values{"namespace": {"   "}})
	if len(frames) != 1 {
		t.Fatalf("invalid namespace = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	if errEnv := wantFrameError(t, frames, 0); errEnv["code"] != "invalid_request" {
		t.Fatalf("invalid namespace code = %v, want invalid_request", errEnv["code"])
	}

	// The stream is GET-only.
	srv := httptest.NewServer(http.HandlerFunc(h.api.HandleSubscribe))
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/subscribe?namespace=rt", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post subscribe: %v", err)
	}
	body := decodeBody(t, res)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405 (%v)", res.StatusCode, body)
	}
	if allow := res.Header.Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

// decodeBody reads and decodes a JSON error response body.
func decodeBody(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	return out
}

// TestSubscribeRouteUnregistered: no route joins the api mux in 6a — the
// endpoint goes public in 6b with its specified live-stream behavior, and
// subscribe is never an Ops entry (§9.2: the stream is an HTTP-surface
// capability like /mcp; the MCP surface gets wait_for).
func TestSubscribeRouteUnregistered(t *testing.T) {
	h := newHarness(t)
	res, err := http.Get(h.srv.URL + "/v1/subscribe?namespace=rt")
	if err != nil {
		t.Fatalf("get /v1/subscribe: %v", err)
	}
	body := decodeBody(t, res)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/subscribe status = %d, want 404 (%v)", res.StatusCode, body)
	}
	if errEnv, _ := body["error"].(map[string]any); errEnv["code"] != "not_found" {
		t.Fatalf("/v1/subscribe code = %v, want not_found", errEnv["code"])
	}
	for _, name := range api.OpNames() {
		if name == "subscribe" {
			t.Fatalf("subscribe must not be an Ops entry — it is an HTTP-surface capability, not an op")
		}
	}
}
