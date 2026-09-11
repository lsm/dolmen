package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lsm/dolmen/internal/store"
)

// HandleSubscribe is §9.2 layer 3's HTTP-surface capability: a GET
// text/event-stream that replays the namespace's durable change log from a
// cursor in commit order and then stays open, delivering live commits. It is
// an HTTP-surface handler like /mcp, not an Ops entry: the MCP tool surface
// gets wait_for, whose request/response shape carries the same feed semantics
// (§2's transport parity), while the stream is for agent hosts holding
// connections.
//
// The live half is the engine's listener, not this handler's loop: Listen
// registers the session and fixes the replay boundary as one operation
// (§9.3's register-and-replay), ChangeReplay.Next pages out the replay, and
// notify delivers what committed after the boundary — so a commit landing
// during the handoff is neither duplicated nor skipped, and the handler only
// frames what the listener hands it. Terminal frames carry the cursor the
// stream reached, so a reconnecting client resumes exactly there.
//
// The route stays UNREGISTERED until 6b lands live streaming. The endpoint's
// specified behavior is a live stream, and a client discovering a registered
// replay-then-terminate route could mistake the terminal frame for
// end-of-subscription and miss every subsequent commit; until then the
// handler is exercised handler-direct only (httptest against it, never
// through the mux), and 6b registers it on the api mux in Server.Handler.
//
// Query params mirror the changes_since op exactly: namespace (required),
// table (optional filter over that table's CURRENT lifetime), and cursor (an
// opaque resume token or the "begin" sentinel; omitted starts at the current
// head — wake-up semantics, a fresh subscriber gets future events only).
// Errors split at the moment the stream opens: request-shape failures (wrong
// method, missing/empty params) are ordinary HTTP errors before any bytes go
// out, while everything the stream discovers — the cursor teaching errors, a
// missing table, engine failures — arrives as an SSE error event carrying the
// standard error envelope inside, because an open text/event-stream response
// can no longer carry an HTTP status.
func (s *Server) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
	// The stream is its own request: it carries a request id like every /v1/
	// call, so an in-stream error envelope, the response header, and the
	// server log line for it can be correlated. 6b's mux entry relies on this
	// assignment happening here.
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
	sseFlush(w)

	ns := normNS(q.Get("namespace"))
	if err := s.ensureNamespace(r.Context(), ns); err != nil {
		sseErrorEvent(w, wrapStoreErr(err), reqID)
		return
	}
	// Both halves run on this goroutine: notify and closed fire on the
	// listener's own goroutines, so each hands its work over a channel and
	// the response is written from exactly one place. The sends are
	// cancellation-guarded — a drain blocked on a subscriber that has gone
	// away would otherwise hold the listener's teardown open, and cancel
	// waits for that drain.
	live := make(chan store.ChangeRecord, 1)
	ended := make(chan error, 1)
	replay, cancel, err := s.eng.Listen(r.Context(), ns, table, cursor, [16]byte{}, nil,
		func(rec store.ChangeRecord) {
			select {
			case live <- rec:
			case <-r.Context().Done():
			}
		},
		func(cause error) {
			select {
			case ended <- cause:
			case <-r.Context().Done():
			}
		})
	if err != nil {
		sseErrorEvent(w, subscribeErr(err), reqID)
		return
	}
	defer cancel()

	resume := replay.Resume()
	write := func(rec store.ChangeRecord) bool {
		resume = rec.Cursor
		return sseEvent(w, "change", sseChange{
			Cursor: string(rec.Cursor),
			Table:  rec.Table,
			RowID:  rec.RowID,
			Kind:   string(rec.Kind),
		})
	}

	// The replay half: pages in cursor order until the boundary call reports
	// done — the last page carrying records returns done=false, the call
	// after it returns done=true — after which notify delivers.
	for {
		records, _, done, nerr := replay.Next(r.Context())
		if nerr != nil {
			if r.Context().Err() != nil {
				return // the subscriber went away; there is nobody to tell
			}
			// The listener may have ended the session under the replay (a
			// dropped target, an evicted namespace), in which case the read
			// fails with the teardown's own error and the cause the listener
			// reported is the one that teaches: prefer it.
			select {
			case cause := <-ended:
				sseEvent(w, "close", sseClose{Cursor: string(resume)})
				sseErrorEvent(w, subscribeErr(cause), reqID)
			default:
				sseErrorEvent(w, subscribeErr(nerr), reqID)
			}
			return
		}
		for _, rec := range records {
			if !write(rec) {
				return // the write failed: the subscriber is gone
			}
		}
		if done {
			break
		}
	}

	// The live half: keep the stream open until the listener ends it or the
	// subscriber disconnects. A disconnect returns here, and the deferred
	// cancel tears the session down — the listener is released.
	for {
		select {
		case rec := <-live:
			if !write(rec) {
				return
			}
		case cause := <-ended:
			sseEvent(w, "close", sseClose{Cursor: string(resume)})
			sseErrorEvent(w, subscribeErr(cause), reqID)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// subscribeErr maps a failure onto the stream's error frame. The cursor
// teaching errors carry their own catch-up path, phrased for this surface —
// reconnecting IS the catch-up call — and stay generic on purpose: which feed
// or table a foreign cursor was minted for is not the caller's to learn here.
// The listener's own ends teach their remedies on the same envelope, so a
// subscriber reads one error shape whichever half ended the stream.
func subscribeErr(err error) *Error {
	switch {
	case errors.Is(err, store.ErrCursorExpired):
		return badRequest("cursor is unknown or past the change-log retention window (-change-retention, default 168h); catch up by reconnecting with no cursor to resume from the current head, or with cursor=begin to replay retained history")
	case errors.Is(err, store.ErrCursorCrossFeed):
		return badRequest("cursor was minted on a different feed (a specific table's, or the namespace-wide feed); pass it only to the feed you received it from — honoring it elsewhere would silently skip events — or start fresh with no cursor / cursor=begin")
	case errors.Is(err, store.ErrListenOverflow):
		return badRequest("subscription buffer overflow: commits arrived faster than this stream drained them; reconnect from the cursor in the preceding close frame — the durable log is the catch-up path, the buffer never was")
	case errors.Is(err, store.ErrListenLifetimeEnded):
		return badRequest("the subscription's target ended (a dropped table, or a dropped or replaced namespace); reconnect against the current target — a same-named successor is a different feed")
	case errors.Is(err, store.ErrListenRevoked):
		return badRequest("subscription authorization was revoked; reconnect once authorization is restored")
	default:
		return wrapStoreErr(err)
	}
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

// sseClose is the terminal frame's cursor handoff: the last position the
// stream delivered, so a client reconnecting from it skips nothing. It
// precedes a teaching error and never follows a healthy stream — a live
// stream has no terminal to announce.
type sseClose struct {
	Cursor string `json:"cursor"`
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

// sseErrorEvent delivers a teaching or failure error as the stream's error
// event: the standard error envelope — the same {ok: false, error: {...}}
// shape every /v1 error responds with — as the event's data, so a subscriber
// reads one error shape on every surface. The error event is terminal; no
// close frame follows it.
func sseErrorEvent(w http.ResponseWriter, apiErr *Error, reqID string) {
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
	sseEvent(w, "error", map[string]any{"ok": false, "error": apiErr.Public(reqID)})
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
