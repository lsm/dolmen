package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/lsm/dolmen/internal/store"
)

// HandleSubscribe is §9.2 layer 3's HTTP-surface capability: a GET
// text/event-stream that replays the namespace's durable change log from a
// cursor in commit order and then STAYS OPEN, delivering live commits — the
// registered route since 6b, driven through the engine's atomic
// register-and-replay (Listen, §6.2/§9.3): registration fixes the replay
// boundary, the replay pages out first, and the notify half takes over at
// exactly the boundary, so a write during replay is neither duplicated nor
// skipped. It is an HTTP-surface handler like /mcp, not an Ops entry: the
// MCP tool surface gets wait_for, whose request/response shape carries the
// same feed semantics (§2's transport parity), while the stream is for agent
// hosts holding connections.
//
// Query params mirror the changes_since op exactly: namespace (required),
// table (optional filter over that table's CURRENT lifetime), and cursor (an
// opaque resume token or the "begin" sentinel; omitted starts at the current
// head — wake-up semantics, a fresh subscriber gets future events only).
// Errors split at the moment the stream opens: request-shape failures (wrong
// method, missing/empty params) are ordinary HTTP errors before any bytes go
// out, while everything the stream discovers — the cursor teaching errors, a
// missing table, engine failures — arrives as an SSE error event carrying
// the standard error envelope inside, because an open text/event-stream
// response can no longer carry an HTTP status.
//
// Once live, two goroutines write frames besides the handler's own replay
// pages — the engine's notify (change frames) and its closed callback (the
// terminal frame) — so every write goes through the stream's mutex, and the
// handler returns only after cancel quiesces the session (no frame, and no
// ResponseWriter access, can follow it).
func (s *Server) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
	// The stream is its own request: it carries a request id like every /v1/
	// call, so an in-stream error envelope, the response header, and the
	// server log line for it can be correlated.
	r = r.WithContext(WithRequestID(r.Context(), RequestIDFor(r)))
	reqID := RequestIDFrom(r.Context())

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, r, &Error{Status: http.StatusMethodNotAllowed, Code: ErrCodeInvalid, Message: "use GET"})
		return
	}
	q := r.URL.Query()
	if q.Get("namespace") == "" {
		writeError(w, r, badRequest("namespace query parameter is required"))
		return
	}
	// Explicitly empty selectors are rejected, never read as omitted — the
	// same rule the changes_since op enforces: an empty table would silently
	// widen the feed to the whole namespace, and an empty cursor would
	// silently swap a resume for a bare head start, skipping the caller's
	// backlog.
	table := ""
	if q.Has("table") {
		table = normTable(q.Get("table"))
		if table == "" {
			writeError(w, r, badRequest("table must be a non-empty table name — omit the parameter for the namespace-wide feed"))
			return
		}
	}
	cursor := store.Cursor("")
	if q.Has("cursor") {
		c := q.Get("cursor")
		if strings.TrimSpace(c) == "" {
			writeError(w, r, badRequest(`cursor must be a non-empty opaque token, or the literal "begin" — omit the parameter to start at the current head`))
			return
		}
		cursor = store.Cursor(c)
	}

	// From here the response IS the stream: the headers go out and flush
	// immediately, and every later failure is an in-stream error event.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Request-Id", reqID)
	w.WriteHeader(http.StatusOK)
	st := &sseStream{w: w}

	ns := normNS(q.Get("namespace"))
	if err := s.ensureNamespace(r.Context(), ns); err != nil {
		st.apiError(wrapStoreErr(err), reqID)
		return
	}

	// The terminal signal: closed fires on the engine's goroutine when IT
	// ends the session, but it must not write the terminal frame directly —
	// the handler may be mid-page writing change frames, and a terminal must
	// never land between them. closed only ARMS the terminal (every later
	// change write stops) and releases the handler below, which flushes the
	// armed frame once its own writes have drained. streamDone takes the
	// place of 6a's close-after-replay — a healthy stream never ends.
	streamDone := make(chan struct{})
	notify := func(rec store.ChangeRecord) {
		// The drainer runs this on the session's goroutine; a failed write
		// means the subscriber is gone — the stream stops writing, and the
		// handler's select tears the session down (cancel must not be called
		// from here: it waits for this very goroutine).
		st.change(rec)
	}
	closed := func(cause error) {
		st.armTerminal(func() {
			switch {
			case errors.Is(cause, store.ErrListenOverflow):
				// The teaching reconnect recipe (§6.2): resume from the
				// persisted cursor — the durable log is the catch-up path;
				// the buffer never was the durability mechanism.
				st.closeFrame(closeRecipe{
					Cursor: string(st.last),
					Reason: "subscription buffer overflow: the stream drained slower than commits arrived; reconnect with your last received cursor to catch up from the durable change log, or start fresh with no cursor / cursor=begin",
				})
			case errors.Is(cause, store.ErrListenLifetimeEnded):
				st.closeFrame(closeRecipe{
					Cursor: string(st.last),
					Reason: "the subscribed table or namespace was dropped mid-stream; reconnect and resubscribe — a same-named successor is a different feed",
				})
			case errors.Is(cause, store.ErrListenRevoked):
				st.closeFrame(closeRecipe{
					Cursor: string(st.last),
					Reason: "authorization for this stream was revoked or narrowed mid-subscription; reconnect once access is restored",
				})
			default:
				st.apiError(wrapStoreErr(cause), reqID)
			}
		})
		close(streamDone)
	}
	// TODO(9d): liveAuthz is nil while auth is off; the slice that resolves
	// per-event scopes passes the API layer's re-resolver here.
	replay, cancel, err := s.eng.Listen(r.Context(), ns, table, cursor,
		[16]byte{}, nil, notify, closed)
	if err != nil {
		if r.Context().Err() != nil {
			return // the subscriber went away; there is nobody to tell
		}
		// The cursor teaching errors carry their own catch-up path,
		// phrased for this surface — reconnecting IS the catch-up call.
		// They stay generic on purpose: which feed or table a foreign
		// cursor was minted for is not the caller's to learn here.
		if errors.Is(err, store.ErrCursorExpired) {
			st.apiError(badRequest("cursor is unknown or past the change-log retention window (-change-retention, default 168h); catch up by reconnecting with no cursor to resume from the current head, or with cursor=begin to replay retained history"), reqID)
			return
		}
		if errors.Is(err, store.ErrCursorCrossFeed) {
			st.apiError(badRequest("cursor was minted on a different feed (a specific table's, or the namespace-wide feed); pass it only to the feed you received it from — honoring it elsewhere would silently skip events — or start fresh with no cursor / cursor=begin"), reqID)
			return
		}
		st.apiError(wrapStoreErr(err), reqID)
		return
	}
	defer cancel()

	// Replay half: page the registration boundary's range out as change
	// frames. done ends the loop exactly at the boundary; a session ended
	// under the pages (overflow, revocation, a dropped feed target) reports
	// done too, with any not-yet-written page dropped — the armed terminal
	// carries why, and reconnecting from the last delivered cursor recovers
	// everything the drop skipped.
loop:
	for {
		records, _, done, err := replay.Next(r.Context())
		if err != nil {
			if r.Context().Err() != nil {
				return // the subscriber went away; there is nobody to tell
			}
			st.apiError(wrapStoreErr(err), reqID)
			return
		}
		for _, rec := range records {
			if !st.change(rec) {
				break loop // the subscriber is gone, or the terminal is armed
			}
		}
		if done {
			break
		}
	}

	// Live half: the engine's callbacks own the wire now — each committed
	// change arrives as a change frame through notify — and the handler only
	// rides the connection until one side ends it: the subscriber
	// disconnecting (cancel below tears the session down cleanly) or the
	// engine ending the stream (the terminal is armed and flushed here, on
	// the handler's goroutine, after every in-flight frame).
	if !st.gone() {
		select {
		case <-r.Context().Done():
		case <-streamDone:
		}
	}
	st.flushTerminal()
}

// sseChange is one change event's data: the same four-field public
// projection the changes_since op returns — identity only, never a row
// snapshot (§9.3) — so both transports teach one shape.
type sseChange struct {
	Cursor string `json:"cursor"`
	Table  string `json:"table"`
	RowID  int64  `json:"row_id"`
	Kind   string `json:"kind"`
}

// closeRecipe is the stream's terminal close frame: the reconnect teaching
// (why the stream ended and how to resume) plus the cursor of the last
// delivered change — the exact position a reconnecting client resumes from,
// empty when nothing was delivered (start fresh at the head).
type closeRecipe struct {
	Cursor string `json:"cursor"`
	Reason string `json:"reason"`
}

// sseStream is the subscribe stream's wire discipline: it serializes frame
// writes between the three goroutines a live subscription runs — the handler
// (replay pages), the engine's notify (change frames), and its closed
// callback — and keeps the terminal LAST. A terminal frame armed by closed
// is flushed by the handler's goroutine only after its in-flight frames
// drain, so no change frame can ever follow the stream's last word; while a
// terminal is armed every change write is refused, so the engine's drainer
// stops too. After any write fails the subscriber is gone; every later write
// (terminal included) is skipped so a dead connection cannot spin the pumps.
type sseStream struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	last     store.Cursor // cursor of the last change frame written
	dead     bool         // a write failed: the subscriber is gone
	terminal func()       // armed by closed, flushed once by the handler
}

// change writes one change frame, updating the resume cursor it carries.
// It reports false once the subscriber is gone or the terminal is armed —
// the caller stops writing either way.
func (st *sseStream) change(rec store.ChangeRecord) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.dead || st.terminal != nil {
		return false
	}
	if !sseEvent(st.w, "change", sseChange{
		Cursor: string(rec.Cursor),
		Table:  rec.Table,
		RowID:  rec.RowID,
		Kind:   string(rec.Kind),
	}) {
		st.dead = true
		return false
	}
	st.last = rec.Cursor
	return true
}

// armTerminal records the stream's last word without writing it — the
// engine's closed callback runs on a pump goroutine, and the terminal must
// not interleave with the handler's in-flight page. First arm wins.
func (st *sseStream) armTerminal(fn func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.terminal == nil {
		st.terminal = fn
	}
}

// flushTerminal writes the armed terminal frame, if any, on the caller's
// (the handler's) goroutine — after every change frame it was writing, and
// with the last delivered cursor read at flush time, so the close frame
// reports exactly what the subscriber could have received.
func (st *sseStream) flushTerminal() {
	st.mu.Lock()
	defer st.mu.Unlock()
	fn := st.terminal
	st.terminal = nil
	if fn == nil || st.dead {
		return
	}
	fn()
}

// gone reports whether the subscriber's connection is unwritable — no
// terminal is worth waiting to flush for it.
func (st *sseStream) gone() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.dead
}

// closeFrame writes the terminal close frame.
func (st *sseStream) closeFrame(recipe closeRecipe) {
	if !sseEvent(st.w, "close", recipe) {
		st.dead = true
	}
}

// apiError writes an in-stream error event. Callers are the handler's own
// goroutine: before the session's pumps exist (the pre-Listen failures), or
// inside an armed terminal's closure (under flushTerminal's lock) — never
// concurrently with the drainer's frames.
func (st *sseStream) apiError(apiErr *Error, reqID string) {
	status := apiErr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	// The same log split writeError makes: server-class failures are
	// operator-visible at Error level with their cause; request-class
	// failures only when debugging.
	if status >= http.StatusInternalServerError {
		slog.Error("sse error", "code", apiErr.Code, "status", status, "request_id", reqID, "cause", apiErr.Cause)
	} else {
		slog.Debug("sse error", "code", apiErr.Code, "status", status, "request_id", reqID, "cause", apiErr.Cause)
	}
	if !sseEvent(st.w, "error", map[string]any{"ok": false, "error": apiErr.Public(reqID)}) {
		st.dead = true
	}
}

// sseEvent frames one server-sent event — named event, single-line JSON
// data, blank-line terminator — and flushes it immediately: a stream frame
// held in a buffer is a frame the subscriber has not received. It reports
// false when the write failed (the subscriber disconnected), so the caller
// can stop instead of spinning on a dead connection.
func sseEvent(w http.ResponseWriter, event string, data any) bool {
	payload, err := sseJSON(data)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return false
	}
	sseFlush(w)
	return true
}

// sseJSON encodes v as exactly one compact line — the SSE data field is
// line-framed, and json.Encoder never emits raw newlines inside its payload,
// so structural newlines are the only ones and they are trimmed. HTML
// escaping is off to match every other JSON surface the server writes.
func sseJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sseFlush pushes buffered frames to the wire. Every ResponseWriter the
// server hands an HTTP/1.1 handler implements http.Flusher; the assert keeps
// an exotic test double from panicking on it.
func sseFlush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
