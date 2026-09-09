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
// cursor in commit order and then closes — the replay half. It is an
// HTTP-surface handler like /mcp, not an Ops entry: the MCP tool surface gets
// wait_for, whose request/response shape carries the same feed semantics
// (§2's transport parity), while the stream is for agent hosts holding
// connections.
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
	from := cursor
	for {
		records, next, err := s.eng.ChangesSince(r.Context(), ns, table, from,
			[16]byte{}, nil, store.Incarnation{}, store.Page{Limit: store.MaxChangesPageLimit})
		if err != nil {
			if r.Context().Err() != nil {
				return // the subscriber went away; there is nobody to tell
			}
			// The cursor teaching errors carry their own catch-up path,
			// phrased for this surface — reconnecting IS the catch-up call.
			// They stay generic on purpose: which feed or table a foreign
			// cursor was minted for is not the caller's to learn here.
			if errors.Is(err, store.ErrCursorExpired) {
				sseErrorEvent(w, badRequest("cursor is unknown or past the change-log retention window (-change-retention, default 168h); catch up by reconnecting with no cursor to resume from the current head, or with cursor=begin to replay retained history"), reqID)
				return
			}
			if errors.Is(err, store.ErrCursorCrossFeed) {
				sseErrorEvent(w, badRequest("cursor was minted on a different feed (a specific table's, or the namespace-wide feed); pass it only to the feed you received it from — honoring it elsewhere would silently skip events — or start fresh with no cursor / cursor=begin"), reqID)
				return
			}
			sseErrorEvent(w, wrapStoreErr(err), reqID)
			return
		}
		for _, rec := range records {
			if !sseEvent(w, "change", sseChange{
				Cursor: string(rec.Cursor),
				Table:  rec.Table,
				RowID:  rec.RowID,
				Kind:   string(rec.Kind),
			}) {
				return // the write failed: the subscriber is gone
			}
		}
		// A short page means the backlog is drained; the close frame carries
		// the boundary cursor so a client persisting it resumes exactly here.
		// (A backlog growing faster than it drains keeps the loop catching up
		// — inherent to replay; 6b's live streaming holds the stream open
		// instead.)
		if len(records) < store.MaxChangesPageLimit {
			sseEvent(w, "close", sseClose{Cursor: string(next)})
			return
		}
		from = next
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

// sseClose is the replay half's terminal frame: the cursor at the replay
// boundary, the exact position a reconnecting client resumes from. 6b
// replaces this frame with live streaming.
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
