package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

// HandleSubscribe is §9.2 layer 3's HTTP-surface capability: a GET
// text/event-stream that replays the namespace's durable change log from a
// cursor in commit order and then delivers live commits through the engine's
// listener. It is an HTTP-surface handler like /mcp, not an Ops entry: the
// MCP tool surface gets wait_for, whose request/response shape carries the
// same feed semantics (§2's transport parity), while the stream is for agent
// hosts holding connections.
//
// The route joins the api mux with the registration slice, not before. The
// endpoint's specified behavior is a live stream, and a client discovering a
// replay-then-terminate route could mistake the terminal frame for
// end-of-subscription and miss every subsequent commit; until then the
// handler is exercised handler-direct only (httptest against it, never
// through the mux).
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
const sseWriteDeadline = 10 * time.Second

func (s *Server) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
	// The stream is its own request: it carries a request id like every /v1/
	// call, so an in-stream error envelope, the response header, and the
	// server log line for it can be correlated. The mux entry relies on this
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

	ns := normNS(q.Get("namespace"))
	ctx, stop := context.WithCancel(r.Context())
	defer stop()
	if err := s.ensureNamespace(ctx, ns); err != nil {
		sseOpenStream(w, reqID)
		sseErrorEvent(w, wrapStoreErr(err), reqID)
		return
	}
	live := make(chan store.ChangeRecord, 1)
	ended := make(chan error, 1)
	replay, cancel, err := s.eng.Listen(ctx, ns, table, cursor, [16]byte{}, nil,
		func(rec store.ChangeRecord) {
			select {
			case live <- rec:
			case <-ctx.Done():
			}
		},
		func(cause error) {
			select {
			case ended <- cause:
			case <-ctx.Done():
			}
		})
	// The response opens only once the listener is registered: a client that
	// observes the open stream must know its subscription exists, or a commit
	// it makes immediately after that observation could land before the
	// boundary and never be delivered. Registration failures still open the
	// stream first, so every in-stream failure keeps one shape.
	sseOpenStream(w, reqID)
	if err != nil {
		sseErrorEvent(w, subscribeErr(err), reqID)
		return
	}
	defer func() {
		stop()
		cancel()
	}()

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

	for {
		records, _, done, nerr := replay.Next(ctx)
		if nerr != nil {
			if ctx.Err() != nil {
				return
			}
			select {
			case cause := <-ended:
				sseEvent(w, "close", sseClose{Cursor: string(resume)})
				sseErrorEvent(w, subscribeErr(cause), reqID)
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				sseEvent(w, "close", sseClose{Cursor: string(resume)})
				sseErrorEvent(w, subscribeErr(nerr), reqID)
			}
			return
		}
		for _, rec := range records {
			if !write(rec) {
				return
			}
		}
		if done {
			break
		}
	}

	for {
		select {
		case rec := <-live:
			if !write(rec) {
				return
			}
		case cause := <-ended:
			for {
				select {
				case rec := <-live:
					if !write(rec) {
						return
					}
				default:
					sseEvent(w, "close", sseClose{Cursor: string(resume)})
					sseErrorEvent(w, subscribeErr(cause), reqID)
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func sseOpenStream(w http.ResponseWriter, reqID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Request-Id", reqID)
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
	w.WriteHeader(http.StatusOK)
	rc.Flush()
	rc.SetWriteDeadline(time.Time{})
}

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

// sseClose is the terminal frame's cursor handoff: the position the
// stream reached, the exact place a reconnecting client resumes from.

type sseClose struct {
	Cursor string `json:"cursor"`
}

// sseEvent frames one server-sent event — named event, single-line JSON
// data, blank-line terminator — and flushes it immediately: a stream frame
// held in a buffer is a frame the subscriber has not received. It reports
// false when the write or the flush failed (the subscriber disconnected —
// a small frame can sit in net/http's buffer and die only at the flush,
// so the flush error is part of the frame's success), so the caller can
// stop instead of spinning on a dead connection.
func sseEvent(w http.ResponseWriter, event string, data any) bool {
	payload, err := sseJSON(data)
	if err != nil {
		return false
	}
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
	_, werr := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	ferr := rc.Flush()
	rc.SetWriteDeadline(time.Time{})
	return werr == nil && ferr == nil
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
