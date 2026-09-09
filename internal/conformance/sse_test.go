package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
)

// Slice 6b conformance (§9.2 layer 3, live half): the subscribe SSE route is
// REGISTERED and serves replay-then-live — registration and replay are one
// atomic engine operation (Listen, §6.2), so a write during replay is
// neither duplicated nor skipped; a slow drain overflows to the teaching
// reconnect frame; a disconnect releases the listener. Streams no longer
// terminate on their own, so the fixtures read frames incrementally off a
// live reader and close the request when done, instead of 6a's read-to-EOF.

// sseFrame is one parsed server-sent event: its named event type and data
// line. Comment lines and retry hints carry neither and are dropped.
type sseFrame struct {
	event string
	data  string
}

// readSSEFrames streams every frame off an already-open response body until
// the handler ends the stream, parsing the wire framing line by line — the
// read a live EventSource performs, not a buffered slurp. For terminal
// streams only: a live stream never ends, and reading it to "the end" would
// hang (the live fixtures use sseLive below).
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

// sseLive is one live subscribe stream read incrementally: frames land on a
// channel as the server writes them, so a test can interleave its own writes
// with the stream's delivery and assert what arrives (and what must not).
type sseLive struct {
	t         *testing.T
	res       *http.Response
	frames    chan sseFrame
	ended     chan error // the reader's outcome; closed after the send, so any number of waits see it
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// subscribeLive opens a subscribe stream over the harness's REGISTERED route
// (6b joins the mux) and returns it with nothing read.
func (h *harness) subscribeLive(t *testing.T, query url.Values) *sseLive {
	t.Helper()
	return subscribeLiveOn(t, h.srv.URL, query)
}

// subscribeLiveOn opens a stream against one explicit server URL — the
// disconnect fixture runs against a server it owns and can close.
func subscribeLiveOn(t *testing.T, base string, query url.Values) *sseLive {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/v1/subscribe?"+query.Encode(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open subscribe stream: %v", err)
	}
	s := &sseLive{
		t: t, res: res, cancel: cancel,
		frames: make(chan sseFrame, 64),
		ended:  make(chan error, 1),
	}
	go s.read()
	t.Cleanup(s.close)
	return s
}

// read parses the stream's wire framing on its own goroutine, one frame at a
// time, and reports the stream's end on ended.
func (s *sseLive) read() {
	defer close(s.frames)
	var cur sseFrame
	sc := bufio.NewScanner(s.res.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if cur.event != "" || cur.data != "" {
				s.frames <- cur
				cur = sseFrame{}
			}
		}
	}
	s.ended <- sc.Err()
	close(s.ended)
	_ = s.res.Body.Close()
}

// next reads one frame, failing on the timeout — a live stream must make
// progress, and a stuck one must fail the test rather than hang it.
func (s *sseLive) next(what string) sseFrame {
	s.t.Helper()
	select {
	case f, ok := <-s.frames:
		if !ok {
			s.t.Fatalf("stream ended while waiting for %s", what)
		}
		return f
	case <-time.After(20 * time.Second):
		s.t.Fatalf("timed out waiting for %s", what)
		return sseFrame{}
	}
}

// quiet asserts nothing arrives within the window — the live stream's
// "nothing more is coming" signal (a healthy stream never ends, so absence
// is the only closed-form negative).
func (s *sseLive) quiet(window time.Duration, what string) {
	s.t.Helper()
	select {
	case f, ok := <-s.frames:
		if ok {
			s.t.Fatalf("unexpected %q frame while expecting %s: %s", f.event, what, f.data)
		}
		s.t.Fatalf("stream ended while expecting %s", what)
	case <-time.After(window):
	}
}

// waitEnded asserts the stream ends cleanly (the handler returned), failing
// on the timeout.
func (s *sseLive) waitEnded(what string) {
	s.t.Helper()
	select {
	case err := <-s.ended:
		if err != nil {
			s.t.Fatalf("stream read error after %s: %v", what, err)
		}
	case <-time.After(20 * time.Second):
		s.t.Fatalf("stream did not end after %s", what)
	}
}

// close disconnects the subscriber and waits for the reader to exit.
func (s *sseLive) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		select {
		case <-s.ended:
		case <-time.After(20 * time.Second):
			s.t.Fatalf("subscribe reader did not exit after disconnect")
		}
	})
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

// TestSubscribeReplayThenLive: the slice's core contract — a subscriber
// holding a cursor receives exactly the events committed after it, in commit
// order, and the stream then STAYS OPEN: no terminal frame, and the next
// commit arrives live (§9.2 layer 3, §9.3).
func TestSubscribeReplayThenLive(t *testing.T) {
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

	s := h.subscribeLive(t, url.Values{"namespace": {"rt"}, "cursor": {head}})
	if ct := s.res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("subscribe content-type = %q, want text/event-stream", ct)
	}
	if s.res.Header.Get("X-Request-Id") == "" {
		t.Fatalf("subscribe stream carries no X-Request-Id")
	}

	want := [][3]any{
		{"notes", missedIDs[0], "insert"},
		{"notes", missedIDs[1], "insert"},
	}
	for i, w := range want {
		if got := sseChangeOf(t, s.next("replayed change")); got != w {
			t.Fatalf("change %d = %v, want %v", i, got, w)
		}
	}
	// The replay drained: no close frame — the stream holds open for live
	// delivery, and nothing else is on the log.
	s.quiet(200*time.Millisecond, "an open stream after replay")

	// The next commit arrives live, exactly once.
	after := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "after"}},
	})
	if got, w := sseChangeOf(t, s.next("live change")), [3]any{"notes", after["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("live change = %v, want %v", got, w)
	}
	s.quiet(200*time.Millisecond, "a quiet log after the live change")
	s.close()
}

// TestSubscribeWriteDuringReplayExactlyOnce: the atomic register-and-replay
// (§6.2/§9.3) — a commit landing while the replay still pages out is
// delivered exactly once, never skipped (it is live) and never duplicated
// (it postdates the boundary). The backlog spans two replay pages so the
// concurrent write lands with certainty mid-replay.
func TestSubscribeWriteDuringReplayExactlyOnce(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	backlog := make([]any, 1200)
	for i := range backlog {
		backlog[i] = map[string]any{"title": "b"}
	}
	h.mustHTTP("insert", map[string]any{"namespace": "rt", "table": "notes", "records": backlog})

	s := h.subscribeLive(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})

	// The first frame proves page one is mid-flight: the write below commits
	// while the replay is still draining.
	sseChangeOf(t, s.next("backlog change"))
	during := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "d1"}, map[string]any{"title": "d2"}},
	})
	duringIDs := during["ids"].([]any)

	// The stream must deliver the whole backlog then the two live events —
	// in cursor order, with no duplication of the boundary window.
	seen := map[float64]int{}
	liveSeen := 0
	duringSet := map[float64]bool{duringIDs[0].(float64): true, duringIDs[1].(float64): true}
	for total := 0; total < 1202; total++ {
		triple := sseChangeOf(t, s.next("change frame"))
		id := triple[1].(float64)
		seen[id]++
		if seen[id] > 1 {
			t.Fatalf("row %v delivered twice across the replay/live boundary", id)
		}
		if duringSet[id] {
			liveSeen++
			if total < 1200 {
				t.Fatalf("the mid-replay commit (row %v) was delivered at position %d, before the replay drained — live delivery is gated on the drained boundary", id, total)
			}
		}
	}
	if liveSeen != 2 {
		t.Fatalf("the mid-replay commit appeared %d times, want exactly 2 events live", liveSeen)
	}
	if len(seen) != 1202 {
		t.Fatalf("delivered %d distinct rows, want 1202 (1200 backlog + 2 live)", len(seen))
	}
	s.close()
}

// TestSubscribeCursorForms: the two pinned cursor forms beside a resume
// token — omitted starts at the current head (wake-up semantics: nothing
// replays, only subsequent commits are delivered live), and "begin" replays
// retained history from the oldest readable boundary then goes live (§9.3).
func TestSubscribeCursorForms(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	first := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}},
	})
	firstIDs := first["ids"].([]any)

	// Omitted cursor: a fresh subscriber gets future events only.
	s := h.subscribeLive(t, url.Values{"namespace": {"rt"}})
	s.quiet(200*time.Millisecond, "a bare start over an unreplayed backlog")

	// The next commit — and only it — is delivered from that head.
	next := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "c"}},
	})
	if got, w := sseChangeOf(t, s.next("head-start live change")), [3]any{"notes", next["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("head-start stream = %v, want only the new event %v", got, w)
	}
	s.close()

	// begin: the whole retained backlog, in commit order, then live.
	s = h.subscribeLive(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	want := [][3]any{
		{"notes", firstIDs[0], "insert"},
		{"notes", firstIDs[1], "insert"},
		{"notes", next["ids"].([]any)[0], "insert"},
	}
	for i, w := range want {
		if got := sseChangeOf(t, s.next("begin replay change")); got != w {
			t.Fatalf("begin change %d = %v, want %v", i, got, w)
		}
	}
	s.quiet(200*time.Millisecond, "a drained begin replay")
	after := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "d"}},
	})
	if got, w := sseChangeOf(t, s.next("post-begin live change")), [3]any{"notes", after["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("post-begin live change = %v, want %v", got, w)
	}
	s.close()
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
	s := h.subscribeLive(t, url.Values{"namespace": {"rt"}, "table": {"notes"}, "cursor": {"begin"}})
	if got, w := sseChangeOf(t, s.next("table-feed change")), [3]any{"notes", notes["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("table feed = %v, want only the notes event %v", got, w)
	}
	s.quiet(200*time.Millisecond, "a quiet table feed")
	// A write to the OTHER table never arrives on this feed.
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "tasks", "records": []any{map[string]any{"title": "t2"}},
	})
	s.quiet(200*time.Millisecond, "a foreign table's event")
	s.close()

	// A table-feed cursor does not resolve on the namespace-wide feed.
	frames := h.subscribeTerminal(t, url.Values{"namespace": {"rt"}, "cursor": {tableFeedCursor(t, h)}})
	if len(frames) != 1 {
		t.Fatalf("cross-feed = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	errEnv := wantFrameError(t, frames, 0)
	if errEnv["code"] != "invalid_request" {
		t.Fatalf("cross-feed code = %v, want invalid_request", errEnv["code"])
	}
	if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "different feed") {
		t.Fatalf("cross-feed message %q does not teach the feed binding", msg)
	}

	// A missing table's feed is not_found, in-stream.
	frames = h.subscribeTerminal(t, url.Values{"namespace": {"rt"}, "table": {"missing"}})
	if len(frames) != 1 {
		t.Fatalf("missing table feed = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	if errEnv := wantFrameError(t, frames, 0); errEnv["code"] != "not_found" {
		t.Fatalf("missing table feed code = %v, want not_found", errEnv["code"])
	}
}

// tableFeedCursor mints a cursor on the notes table feed (a table-feed token
// for the cross-feed rejection above).
func tableFeedCursor(t *testing.T, h *harness) string {
	t.Helper()
	return nextCursorOf(t, h.mustHTTP("changes_since", map[string]any{
		"namespace": "rt", "table": "notes", "cursor": "begin",
	}))
}

// subscribeTerminal opens a stream expected to END on its own (an error
// event) and reads it to the end — the 6a shape, for the teaching errors.
func (h *harness) subscribeTerminal(t *testing.T, query url.Values) []sseFrame {
	t.Helper()
	res, err := http.Get(h.srv.URL + "/v1/subscribe?" + query.Encode())
	if err != nil {
		t.Fatalf("open subscribe stream: %v", err)
	}
	return readSSEFrames(t, res)
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
	frames := h.subscribeTerminal(t, url.Values{"namespace": {"rt"}, "cursor": {cursor}})
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
	frames = h.subscribeTerminal(t, url.Values{"namespace": {"rt"}, "cursor": {"never-minted"}})
	if len(frames) != 1 {
		t.Fatalf("unknown cursor = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	if errEnv := wantFrameError(t, frames, 0); errEnv["code"] != "invalid_request" {
		t.Fatalf("unknown cursor code = %v, want invalid_request", errEnv["code"])
	}

	// Both catch-up paths work on the stream itself: a bare start goes live
	// at the head with nothing replayed, and begin replays from the retained
	// boundary — a fresh commit made after the expiry, since the aged-out
	// record is beyond every replay guarantee now.
	s := h.subscribeLive(t, url.Values{"namespace": {"rt"}})
	s.quiet(200*time.Millisecond, "a bare start after the expiry")
	fresh := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "fresh"}},
	})
	if got, w := sseChangeOf(t, s.next("bare-start live change")), [3]any{"notes", fresh["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("bare-start live change = %v, want %v", got, w)
	}
	s.close()

	s = h.subscribeLive(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	if got, w := sseChangeOf(t, s.next("post-error begin replay")), [3]any{"notes", fresh["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("post-error begin replay = %v, want only the fresh event %v", got, w)
	}
	s.quiet(200*time.Millisecond, "a drained post-error begin replay")
	s.close()
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
		res, err := http.Get(h.srv.URL + "/v1/subscribe?" + tt.query.Encode())
		if err != nil {
			t.Fatalf("%s: open stream: %v", tt.name, err)
		}
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
	frames := h.subscribeTerminal(t, url.Values{"namespace": {"   "}})
	if len(frames) != 1 {
		t.Fatalf("invalid namespace = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	if errEnv := wantFrameError(t, frames, 0); errEnv["code"] != "invalid_request" {
		t.Fatalf("invalid namespace code = %v, want invalid_request", errEnv["code"])
	}

	// The stream is GET-only.
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/subscribe?namespace=rt", nil)
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

// TestSubscribeRouteRegistered: 6b joins the stream to the api mux — the
// endpoint goes public exactly when its specified live-stream behavior
// exists — and subscribe is never an Ops entry (§9.2: the stream is an
// HTTP-surface capability like /mcp; the MCP surface gets wait_for).
func TestSubscribeRouteRegistered(t *testing.T) {
	h := newHarness(t)
	h.ensureNS("rt")

	res, err := http.Get(h.srv.URL + "/v1/subscribe?namespace=rt")
	if err != nil {
		t.Fatalf("get /v1/subscribe: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body := decodeBody(t, res)
		t.Fatalf("/v1/subscribe status = %d, want 200 (%v)", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("/v1/subscribe content-type = %q, want text/event-stream", ct)
	}
	for _, name := range api.OpNames() {
		if name == "subscribe" {
			t.Fatalf("subscribe must not be an Ops entry — it is an HTTP-surface capability, not an op")
		}
	}
}

// TestSubscribeOverflowCloseFrame: a subscriber that drains slower than its
// own visible commits arrive gets the teaching close — the reconnect recipe
// carrying the last delivered cursor — and the stream ends there (§6.2: the
// durable log is the catch-up path; the buffer never is). The subscriber
// stops reading while one bulk commit lands, so the engine's bounded buffer
// — not the client — is what gives.
func TestSubscribeOverflowCloseFrame(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	// A bare start: the replay is empty, the drainer runs immediately, and
	// the unread socket is the slow drain.
	s := h.subscribeLive(t, url.Values{"namespace": {"rt"}})

	bulk := make([]any, 9000)
	for i := range bulk {
		bulk[i] = map[string]any{"title": "flood"}
	}
	h.mustHTTP("insert", map[string]any{"namespace": "rt", "table": "notes", "records": bulk})

	// Read to the terminal: change frames until the close, never an error.
	changes := 0
	var closeData map[string]any
	for closeData == nil {
		f := s.next("a frame after the flood")
		switch f.event {
		case "change":
			changes++
		case "close":
			closeData = frameData(t, f)
		default:
			t.Fatalf("unexpected %q event mid-overflow: %s", f.event, f.data)
		}
		if changes > 9000 {
			t.Fatalf("delivered %d change frames with no overflow close — the bound never tripped", changes)
		}
	}
	reason, _ := closeData["reason"].(string)
	for _, teach := range []string{"overflow", "cursor"} {
		if !strings.Contains(reason, teach) {
			t.Fatalf("close reason %q does not teach the reconnect path (%q missing)", reason, teach)
		}
	}
	cursor, _ := closeData["cursor"].(string)
	if cursor == "" {
		t.Fatalf("overflow close carries no resume cursor: %v", closeData)
	}
	if len(closeData) != 2 {
		t.Fatalf("close frame carries fields beyond cursor/reason: %v", closeData)
	}
	// The close is terminal: the stream ends.
	s.waitEnded("the overflow close frame")
}

// TestSubscribeDisconnectReleasesListener: a client disconnect mid-stream
// cancels the session cleanly — the handler returns (a server Close blocks
// until it does), and later commits to the namespace succeed without the
// departed subscriber (no hang, no leak).
func TestSubscribeDisconnectReleasesListener(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	inserted := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "x"}},
	})

	// A server this test owns and can close: Close blocks until the stream's
	// handler returns, which is exactly the assertion.
	srv := httptest.NewServer(api.OriginGuard(h.api.Handler(), nil))
	s := subscribeLiveOn(t, srv.URL, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	if got, w := sseChangeOf(t, s.next("replayed change")), [3]any{"notes", inserted["ids"].([]any)[0], "insert"}; got != w {
		t.Fatalf("replayed change = %v, want %v", got, w)
	}

	// Disconnect. The handler must return: closing the server waits for
	// outstanding handlers, so its Close completing is the proof.
	s.cancel()
	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("subscribe handler did not return after the client disconnected")
	}

	// The namespace still serves writers with the subscriber gone, and the
	// cancelled stream's reader exits.
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "y"}},
	})
	select {
	case <-s.ended:
	case <-time.After(20 * time.Second):
		t.Fatal("subscribe reader did not exit after disconnect")
	}
}
