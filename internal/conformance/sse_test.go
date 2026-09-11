package conformance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/store"
)

// Slice 6b conformance (§9.2 layer 3): the subscribe SSE stream runs
// replay-then-live through the engine's listener — replay in cursor order,
// then live commits on the same open stream, exactly-once across the
// boundary, with the listener's ends teaching their reconnect. A live stream
// has no terminal to read to, so every read here is bounded: the tests
// assert a prefix and then disconnect. The route itself goes public with the
// registration slice; until then every stream is handler-direct (httptest
// against the handler, never through the mux).

// sseFrame is one parsed server-sent event: its named event type and data
// line. Comment lines and retry hints carry neither and are dropped.
type sseFrame struct {
	event string
	data  string
}

type sseReader struct {
	frames chan sseFrame
}

func newSSEReader(res *http.Response) *sseReader {
	r := &sseReader{frames: make(chan sseFrame, 8)}
	go func() {
		defer close(r.frames)
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
					r.frames <- cur
					cur = sseFrame{}
				}
			}
		}
	}()
	return r
}

func (r *sseReader) next(within time.Duration) (sseFrame, bool) {
	select {
	case f, ok := <-r.frames:
		return f, ok
	case <-time.After(within):
		return sseFrame{}, false
	}
}

func (r *sseReader) rest(within time.Duration) []sseFrame {
	var out []sseFrame
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-r.frames:
			if !ok {
				return out
			}
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
}

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

func (h *harness) subscribeStream(t *testing.T, query url.Values) *sseReader {
	t.Helper()
	res := h.subscribe(t, query)
	t.Cleanup(func() { res.Body.Close() })
	return newSSEReader(res)
}

func (h *harness) insertBatches(t *testing.T, ns, table string, n int) []any {
	t.Helper()
	var ids []any
	for n > 0 {
		batch := n
		if batch > store.MaxChangesPageLimit {
			batch = store.MaxChangesPageLimit
		}
		records := make([]any, 0, batch)
		for i := 0; i < batch; i++ {
			records = append(records, map[string]any{"title": "bulk"})
		}
		res := h.mustHTTP("insert", map[string]any{"namespace": ns, "table": table, "records": records})
		ids = append(ids, res["ids"].([]any)...)
		n -= batch
	}
	return ids
}

func (h *harness) httpData(op string, body any) (map[string]any, error) {
	status, out := h.httpCall(op, body)
	if status != http.StatusOK || out["ok"] != true {
		return nil, fmt.Errorf("/v1/%s failed: status %d %v", op, status, out)
	}
	data, ok := out["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("/v1/%s returned no data object: %v", op, out)
	}
	return data, nil
}

func (h *harness) flood(ns, table string, batches int, done chan<- error) {
	for i := 0; i < batches; i++ {
		records := make([]any, 0, store.MaxChangesPageLimit)
		for j := 0; j < store.MaxChangesPageLimit; j++ {
			records = append(records, map[string]any{"title": "flood"})
		}
		if _, err := h.httpData("insert", map[string]any{"namespace": ns, "table": table, "records": records}); err != nil {
			done <- fmt.Errorf("flood insert %d: %w", i, err)
			return
		}
	}
	done <- nil
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

func wantChange(t *testing.T, f sseFrame, want [3]any) {
	t.Helper()
	if got := sseChangeOf(t, f); got != want {
		t.Fatalf("change = %v, want %v", got, want)
	}
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
// order (§9.2 layer 3, §9.3).
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

	res := h.subscribe(t, url.Values{"namespace": {"rt"}, "cursor": {head}})
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("subscribe content-type = %q, want text/event-stream", ct)
	}
	if res.Header.Get("X-Request-Id") == "" {
		t.Fatalf("subscribe stream carries no X-Request-Id")
	}
	defer res.Body.Close()
	r := newSSEReader(res)

	want := [][3]any{
		{"notes", missedIDs[0], "insert"},
		{"notes", missedIDs[1], "insert"},
	}
	for i, w := range want {
		f, ok := r.next(5 * time.Second)
		if !ok {
			t.Fatalf("replay change %d never arrived on the stream", i)
		}
		wantChange(t, f, w)
	}

	after := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "after"}},
	})
	f, ok := r.next(5 * time.Second)
	if !ok {
		t.Fatal("a commit after the boundary never reached the open stream")
	}
	wantChange(t, f, [3]any{"notes", after["ids"].([]any)[0], "insert"})

	if dup, ok := r.next(300 * time.Millisecond); ok {
		t.Fatalf("the stream delivered an unexpected or duplicate frame: %+v", dup)
	}

	cursor, _ := frameData(t, f)["cursor"].(string)
	r2 := h.subscribeStream(t, url.Values{"namespace": {"rt"}, "cursor": {cursor}})
	later := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "later"}},
	})
	f2, ok := r2.next(5 * time.Second)
	if !ok {
		t.Fatal("the resumed stream never delivered the next commit")
	}
	wantChange(t, f2, [3]any{"notes", later["ids"].([]any)[0], "insert"})
}

// TestSubscribeCursorForms: the two pinned cursor forms beside a resume
// token — omitted starts at the current head (wake-up semantics: nothing
// replays, and only subsequent commits are delivered), and "begin" replays
// retained history from the oldest readable boundary (§9.3).
func TestSubscribeCursorForms(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	first := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes",
		"records": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}},
	})
	firstIDs := first["ids"].([]any)

	// Omitted cursor: a fresh subscriber gets future events only.
	r := h.subscribeStream(t, url.Values{"namespace": {"rt"}})

	next := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "c"}},
	})
	f, ok := r.next(5 * time.Second)
	if !ok {
		t.Fatal("the bare-start stream never delivered the commit")
	}
	wantChange(t, f, [3]any{"notes", next["ids"].([]any)[0], "insert"})

	// begin: the whole retained backlog, in commit order.
	r2 := h.subscribeStream(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	want := [][3]any{
		{"notes", firstIDs[0], "insert"},
		{"notes", firstIDs[1], "insert"},
		{"notes", next["ids"].([]any)[0], "insert"},
	}
	for i, w := range want {
		f, ok := r2.next(5 * time.Second)
		if !ok {
			t.Fatalf("begin replay change %d never arrived", i)
		}
		wantChange(t, f, w)
	}
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

	r := h.subscribeStream(t, url.Values{"namespace": {"rt"}, "table": {"notes"}, "cursor": {"begin"}})
	f, ok := r.next(5 * time.Second)
	if !ok {
		t.Fatal("the table feed never delivered its event")
	}
	wantChange(t, f, [3]any{"notes", notes["ids"].([]any)[0], "insert"})
	tableCursor, _ := frameData(t, f)["cursor"].(string)
	h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "tasks", "records": []any{map[string]any{"title": "t2"}},
	})
	if extra, ok := r.next(300 * time.Millisecond); ok {
		t.Fatalf("the table feed delivered a foreign table's event: %+v", extra)
	}

	// A missing table's feed is not_found, in-stream.
	frames := h.subscribeStream(t, url.Values{"namespace": {"rt"}, "table": {"missing"}}).rest(5 * time.Second)
	if len(frames) != 1 {
		t.Fatalf("missing table feed = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	errEnv := wantFrameError(t, frames, 0)
	if errEnv["code"] != "not_found" {
		t.Fatalf("missing table feed code = %v, want not_found", errEnv["code"])
	}

	// A table-feed cursor does not resolve on the namespace-wide feed.
	frames = h.subscribeStream(t, url.Values{"namespace": {"rt"}, "cursor": {tableCursor}}).rest(5 * time.Second)
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

	frames := h.subscribeStream(t, url.Values{"namespace": {"rt"}, "cursor": {cursor}}).rest(5 * time.Second)
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
	frames = h.subscribeStream(t, url.Values{"namespace": {"rt"}, "cursor": {"never-minted"}}).rest(5 * time.Second)
	if len(frames) != 1 {
		t.Fatalf("unknown cursor = %d frames, want 1 error event: %+v", len(frames), frames)
	}
	if errEnv := wantFrameError(t, frames, 0); errEnv["code"] != "invalid_request" {
		t.Fatalf("unknown cursor code = %v, want invalid_request", errEnv["code"])
	}

	r := h.subscribeStream(t, url.Values{"namespace": {"rt"}})
	fresh := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "fresh"}},
	})
	f, ok := r.next(5 * time.Second)
	if !ok {
		t.Fatal("the post-error stream never delivered the fresh commit")
	}
	wantChange(t, f, [3]any{"notes", fresh["ids"].([]any)[0], "insert"})

	r2 := h.subscribeStream(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	f2, ok := r2.next(5 * time.Second)
	if !ok {
		t.Fatal("the post-error begin replay never delivered the fresh commit")
	}
	wantChange(t, f2, [3]any{"notes", fresh["ids"].([]any)[0], "insert"})
}

func TestSubscribeOverflowTeachesReconnect(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})

	r := h.subscribeStream(t, url.Values{"namespace": {"rt"}})
	probe := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "probe"}},
	})
	f, ok := r.next(10 * time.Second)
	if !ok {
		t.Fatal("the stream never delivered a live record, so the listener was not registered")
	}
	wantChange(t, f, [3]any{"notes", probe["ids"].([]any)[0], "insert"})

	flood := make(chan error, 1)
	go h.flood("rt", "notes", 9, flood)

	frames := r.rest(30 * time.Second)
	if err := <-flood; err != nil {
		t.Fatal(err)
	}
	if len(frames) < 2 {
		t.Fatalf("overflowed stream = %d frames, want a cursor handoff and a teaching error: %+v", len(frames), frames)
	}
	last := frames[len(frames)-1]
	errEnv := wantFrameError(t, frames, len(frames)-1)
	if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "overflow") {
		t.Fatalf("overflow terminal message %q does not teach the buffer bound", msg)
	}
	handoff := wantFrame(t, frames, len(frames)-2, "close")
	if cursor, _ := frameData(t, handoff)["cursor"].(string); cursor == "" {
		t.Fatalf("the overflow terminal carried no resume cursor: %+v", handoff)
	}
	if last.event != "error" {
		t.Fatalf("the stream's last frame is %q, want the teaching error", last.event)
	}
}

func TestSubscribeDisconnectLeavesServerHealthy(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})

	res := h.subscribe(t, url.Values{"namespace": {"rt"}})
	r := newSSEReader(res)
	if _, ok := r.next(200 * time.Millisecond); ok {
		t.Fatal("a bare start replayed a frame")
	}
	res.Body.Close()

	after := h.mustHTTP("insert", map[string]any{
		"namespace": "rt", "table": "notes", "records": []any{map[string]any{"title": "after-disconnect"}},
	})
	r2 := h.subscribeStream(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	f, ok := r2.next(5 * time.Second)
	if !ok {
		t.Fatal("a subscription after a disconnect never delivered the commit")
	}
	wantChange(t, f, [3]any{"notes", after["ids"].([]any)[0], "insert"})
}

func TestSubscribeNamespaceDropTeachesLifetimeEnd(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})
	h.insertBatches(t, "rt", "notes", 2*store.MaxChangesPageLimit)

	r := h.subscribeStream(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	if _, ok := r.next(10 * time.Second); !ok {
		t.Fatal("the replay never started, so the drop does not race it")
	}
	status, body := h.httpCall("drop_namespace", map[string]any{"namespace": "rt", "confirm": "rt"})
	if status != http.StatusOK {
		t.Fatalf("drop namespace: status %d (%v)", status, body)
	}

	frames := r.rest(15 * time.Second)
	if len(frames) < 2 {
		t.Fatalf("dropped-target stream = %d frames, want a cursor handoff and a teaching error: %+v", len(frames), frames)
	}
	errEnv := wantFrameError(t, frames, len(frames)-1)
	if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "target ended") {
		t.Fatalf("dropped-target terminal %q does not teach the lifetime end — the incidental read error was framed instead", msg)
	}
	handoff := wantFrame(t, frames, len(frames)-2, "close")
	if cursor, _ := frameData(t, handoff)["cursor"].(string); cursor == "" {
		t.Fatalf("the terminal carried no resume cursor: %+v", handoff)
	}
}

// TestSubscribeBoundaryUnderConcurrentWrites: the exactly-once property at
// the replay→live handoff while commits race the stream. The writer is
// released on the first client-visible replay frame, which proves
// registration but by itself not that replay is still active — a small
// backlog could stream to completion inside the reader's buffers before the
// writer's commits land, and the test would pass vacuously. The backlog is
// therefore sized far past every buffer between handler and client (40 ×
// the page limit ≈ 4.4 MB of frames, well over the socket buffers, and
// ~10^5 syscall-bound writes the handler cannot finish in the milliseconds
// the four inserts take), so the commits provably land while replay is
// still streaming in every schedule. A server-side hook to hold Replay.Next
// would pin the same window, but the conformance layer drives the handler
// over HTTP and carries no such seam.
func TestSubscribeBoundaryUnderConcurrentWrites(t *testing.T) {
	h := newHarness(t)
	h.seedTable("rt", "notes", []map[string]any{{"name": "title", "type": "string"}})

	seeded := h.insertBatches(t, "rt", "notes", 40*store.MaxChangesPageLimit)

	type batch struct {
		ids []any
		err error
	}
	batches := make(chan batch, 4)
	release := make(chan struct{})
	go func() {
		defer close(batches)
		<-release
		for i := 0; i < 4; i++ {
			data, err := h.httpData("insert", map[string]any{
				"namespace": "rt", "table": "notes",
				"records": []any{map[string]any{"title": "live"}},
			})
			if err != nil {
				batches <- batch{err: fmt.Errorf("live insert %d: %w", i, err)}
				return
			}
			ids, ok := data["ids"].([]any)
			if !ok {
				batches <- batch{err: fmt.Errorf("live insert %d: no ids in %v", i, data)}
				return
			}
			batches <- batch{ids: ids}
		}
	}()

	r := h.subscribeStream(t, url.Values{"namespace": {"rt"}, "cursor": {"begin"}})
	first, ok := r.next(10 * time.Second)
	if !ok {
		t.Fatal("the replay never started, so nothing races the boundary")
	}
	close(release)

	var want []any
	want = append(want, seeded...)
	for b := range batches {
		if b.err != nil {
			t.Fatal(b.err)
		}
		want = append(want, b.ids...)
	}
	got := []any{frameData(t, first)["row_id"]}
	for i := len(got); i < len(want); i++ {
		f, ok := r.next(60 * time.Second)
		if !ok {
			t.Fatalf("stream ended after %d of %d records — the boundary skipped the rest", len(got), len(want))
		}
		dt := frameData(t, f)
		got = append(got, dt["row_id"])
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d = %v, want %v — the boundary duplicated or reordered a record", i, got[i], want[i])
		}
	}
	if extra, ok := r.next(300 * time.Millisecond); ok {
		t.Fatalf("the stream delivered a record beyond the expected set: %+v", extra)
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
	frames := h.subscribeStream(t, url.Values{"namespace": {"   "}}).rest(5 * time.Second)
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

// TestSubscribeRouteUnregistered: no route joins the api mux yet — the
// endpoint goes public with its registration slice, and subscribe is never
// an Ops entry (§9.2: the stream is an HTTP-surface capability like /mcp; the
// MCP surface gets wait_for).
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
