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
	"sync"
	"time"

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
	// immediately, and every later failure is an in-stream error event. The
	// flush is load-bearing: a fresh subscription with no replay backlog
	// writes no frame until its first live commit, and an unflushed header
	// line would leave the client's stream establishment (http.Client.Do, an
	// EventSource open) waiting on a response the handler is already
	// serving — the subscriber could never reach the code that commits.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Request-Id", reqID)
	w.WriteHeader(http.StatusOK)
	if !sseFlush(w) {
		return // the wire is already unwritable; there is nobody to tell
	}
	st := &sseStream{w: w, fallback: cursor, failed: make(chan struct{})} // a terminal before any frame resumes the client exactly where it presented

	// The session handle and the terminal machinery, initialized BEFORE the
	// watcher launches so the goroutine only ever reads immutably-set
	// values: a late-assigned armShutdown raced the handler's write (and
	// could be observed nil — canceling without arming, ending a connected
	// client's stream with no terminal frame at all).
	var replay *store.ChangeReplay
	var cancel func()
	// resumeCursor is the terminal frames' teaching: the last delivered
	// record's cursor, else the trail (the client's presented cursor, then
	// each fully-written page boundary), else the ENGINE's standing cursor
	// — minted at registration — for a stream ended before its first page:
	// a cursor-less terminal there would send the reconnecting client to
	// the newer head and skip every in-flight commit after registration.
	// Before registration exists, the presented cursor is the honest
	// resume (the client reconnects with what it came with).
	resumeCursor := func() store.Cursor {
		if st.last != "" {
			return st.last
		}
		if st.fallback != "" {
			return st.fallback
		}
		if replay != nil {
			return replay.Resume()
		}
		return ""
	}
	// The shutdown terminal, armed by the watcher BEFORE it cancels the
	// stream context: the cancellation races the engine's error close (the
	// fill's aborted database call reports context.Canceled and fires
	// closed with a generic error frame), and first-arm-wins must belong to
	// the teaching close.
	armShutdown := func() {
		st.armTerminal(func() {
			resume := resumeCursor()
			st.closeFrame(closeRecipe{
				Cursor: string(resume),
				Reason: "the server is shutting down; reconnect with your last received cursor to resume from the durable change log (no cursor / cursor=begin starts fresh)",
			})
		})
	}
	// The stream context ends with EITHER the client's request or the
	// shutdown signal: http.Server.Shutdown cancels neither, and the
	// store's mutex and write-lock waits are documented unbounded. It is
	// created BEFORE namespace setup — ensureNamespace can queue behind a
	// DropNamespace pool drain for as long as it takes, and the handler
	// must not outlive main's shutdown deadline waiting for it. The engine
	// derives the session's cancellation scope from this context (the
	// pumps' database work aborts with it), and the pages ride it too.
	streamCtx, streamCancel := context.WithCancel(r.Context())
	defer streamCancel()
	go func() {
		select {
		case <-s.shuttingDown():
			armShutdown()
			streamCancel()
		case <-streamCtx.Done():
		}
	}()

	ns := normNS(q.Get("namespace"))
	if err := s.ensureNamespace(streamCtx, ns); err != nil {
		if streamCtx.Err() != nil {
			return // canceled mid-setup (shutdown or departure); nobody is waiting on teaching
		}
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
		st.changeLive(rec)
	}
	closed := func(cause error) {
		st.armTerminal(func() {
			// Under flushTerminal's lock (resumeCursor reads the fields).
			resume := resumeCursor()
			switch {
			case errors.Is(cause, store.ErrListenOverflow):
				// The teaching reconnect recipe (§6.2): resume from the
				// persisted cursor — the durable log is the catch-up path;
				// the buffer never was the durability mechanism.
				st.closeFrame(closeRecipe{
					Cursor: string(resume),
					Reason: "subscription buffer overflow: the stream drained slower than commits arrived; reconnect with your last received cursor to catch up from the durable change log, or start fresh with no cursor / cursor=begin",
				})
			case errors.Is(cause, store.ErrListenLifetimeEnded):
				st.closeFrame(closeRecipe{
					Cursor: string(resume),
					Reason: "the subscribed table or namespace was dropped mid-stream; reconnect and resubscribe — a same-named successor is a different feed",
				})
			case errors.Is(cause, store.ErrListenRevoked):
				st.closeFrame(closeRecipe{
					Cursor: string(resume),
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
	var err error
	replay, cancel, err = s.eng.Listen(streamCtx, ns, table, cursor,
		[16]byte{}, nil, notify, closed)
	if err != nil {
		if r.Context().Err() != nil {
			return // the subscriber went away; there is nobody to tell
		}
		select {
		case <-s.shuttingDown():
			return // the server is stopping mid-registration; nobody is waiting on this stream's teaching
		default:
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
	// carries why, and reconnecting from the resume cursor recovers
	// everything the drop skipped. Each fully-written page advances the
	// resume trail: the terminal reports the last DELIVERED record's cursor
	// when frames went out, and otherwise the newest page boundary — an
	// empty terminal cursor would send the reconnecting client to the
	// current head and skip the undelivered backlog entirely.
	//
	// The shutdown terminal is armed from the watcher above and from TWO
	// points here — between pages and between frames — because the shutdown
	// signal closes no request context: a big backlog (or a slow reader
	// whose writes block) would otherwise page straight past main's
	// shutdown deadline before the select ever ran. First arm wins; the
	// handler flushes whatever is armed after its in-flight frames.
loop:
	for {
		select {
		case <-s.shuttingDown():
			armShutdown()
			break loop
		default:
		}
		records, next, done, err := replay.Next(streamCtx)
		if err != nil {
			if r.Context().Err() != nil {
				return // the subscriber went away; there is nobody to tell
			}
			if streamCtx.Err() != nil {
				// Shutdown preempted an in-flight page; arm the terminal and
				// fall through to its flush rather than erroring.
				armShutdown()
				break loop
			}
			// A replay that outlived retention (its un-paged backlog pruned
			// out from under it) teaches the same catch-up path the cursor
			// errors at registration do — the durable log is the recovery.
			if errors.Is(err, store.ErrCursorExpired) {
				st.apiError(badRequest("cursor is unknown or past the change-log retention window (-change-retention, default 168h); catch up by reconnecting with no cursor to resume from the current head, or with cursor=begin to replay retained history"), reqID)
				return
			}
			st.apiError(wrapStoreErr(err), reqID)
			return
		}
		for _, rec := range records {
			select {
			case <-s.shuttingDown():
				armShutdown()
				break loop
			default:
			}
			if !st.change(rec) {
				break loop // the subscriber is gone, or the terminal is armed
			}
		}
		st.advanceTrail(next)
		if done {
			break
		}
	}

	// Live half: the engine's callbacks own the wire now — each committed
	// change arrives as a change frame through notify — and the handler only
	// rides the connection until one side ends it: the subscriber
	// disconnecting (cancel below tears the session down cleanly), the engine
	// ending the stream (the terminal is armed and flushed here, on the
	// handler's goroutine, after every in-flight frame), or the SERVER
	// shutting down (Shutdown's signal — http.Server.Shutdown waits for this
	// handler without ever cancelling its request context, so the stream must
	// end itself or every graceful restart waits out its deadline). The
	// drainer stops at the armed terminal, and the close frame lands after
	// every in-flight frame with the reconnect teaching.
	if !st.gone() {
		select {
		case <-r.Context().Done():
		case <-streamDone:
		case <-s.shuttingDown():
			armShutdown()
		case <-st.failed:
			// A frame write failed mid-live (a stalled reader whose socket
			// stayed open): the subscriber is gone; fall through to cancel —
			// no terminal is worth writing on a dead wire.
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
// is flushed by the handler's goroutine only after its own in-flight frames
// drain, so no change frame can ever follow the stream's last word. The two
// change writers carry different duties once a terminal is armed: the
// handler may keep finishing its current page (those frames precede ITS
// flush by construction), while the drainer must stop immediately (its
// frames could otherwise race past the flush). After any write fails the
// subscriber is gone; every later write (terminal included) is skipped so a
// dead connection cannot spin the pumps.
type sseStream struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	last     store.Cursor // cursor of the last change frame written
	fallback store.Cursor // resume trail for a terminal before any frame: the client's presented cursor, then each fully-written page's boundary
	dead     bool         // a write failed: the subscriber is gone
	terminal func()       // armed by closed, flushed once by the handler

	// failed closes (once) when a frame write fails: a stalled reader's
	// socket stays open, so neither the request context nor the engine
	// signals the handler — without this, the handler blocks in its live
	// select forever while the registered session mints and no-ops over
	// every later commit.
	failedOnce sync.Once
	failed     chan struct{}
}

// writeFailed marks the stream unwritable and wakes the handler's live
// select: the subscriber is gone (a failed write), but a stalled TCP peer
// cancels nothing on its own.
func (st *sseStream) writeFailed() {
	st.failedOnce.Do(func() { close(st.failed) })
}

// advanceTrail records a page boundary as the resume fallback — used only
// when no change frame ever went out (a filtered-empty replay, a session
// ended mid-replay): the terminal then teaches reconnection from exactly
// where the replay stood instead of an empty cursor that would skip the
// backlog. Called after the page's records are fully written, so the trail
// never points past an unwritten frame.
func (st *sseStream) advanceTrail(next store.Cursor) {
	st.mu.Lock()
	st.fallback = next
	st.mu.Unlock()
}

// change writes one change frame on the handler's goroutine (the replay
// pages), updating the resume cursor it carries. It reports false once the
// subscriber is gone.
func (st *sseStream) change(rec store.ChangeRecord) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.dead {
		return false
	}
	if !sseEvent(st.w, "change", sseChange{
		Cursor: string(rec.Cursor),
		Table:  rec.Table,
		RowID:  rec.RowID,
		Kind:   string(rec.Kind),
	}) {
		st.dead = true
			st.writeFailed()
		return false
	}
	st.last = rec.Cursor
	return true
}

// changeLive writes one change frame from the engine's drainer. Unlike the
// handler's own writes, it must refuse once a terminal is armed: the
// drainer's frames have no ordering relationship to the handler's flush, and
// a change frame after the stream's last word would corrupt the terminal's
// teaching.
func (st *sseStream) changeLive(rec store.ChangeRecord) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.dead || st.terminal != nil {
		return
	}
	if !sseEvent(st.w, "change", sseChange{
		Cursor: string(rec.Cursor),
		Table:  rec.Table,
		RowID:  rec.RowID,
		Kind:   string(rec.Kind),
	}) {
		st.dead = true
			st.writeFailed()
		return
	}
	st.last = rec.Cursor
}

// armTerminal records the stream's last word without writing it — the
// engine's closed callback runs on a pump goroutine, and the terminal must
// not interleave with the handler's in-flight page. First arm wins. A
// blocked frame write holding the stream mutex is NOT aborted from here:
// a past write deadline resets an HTTP/2 stream outright (the later fresh
// deadline in sseEvent cannot revive it), so a healthy stream would lose
// its terminal to a protocol error. The per-write bound alone carries the
// shutdown case — sseWriteTimeout sits under main's five-second graceful
// budget, so the worst a stopped reader costs the terminal is that bound.
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

// closeFrame writes the terminal close frame, on the terminal's tighter
// bound: a stalled change frame (up to sseWriteTimeout) ahead of it must
// leave room inside main's five-second shutdown budget.
func (st *sseStream) closeFrame(recipe closeRecipe) {
	if !sseEventBounded(st.w, "close", recipe, sseTerminalWriteTimeout) {
		st.dead = true
			st.writeFailed()
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
	if !sseEventBounded(st.w, "error", map[string]any{"ok": false, "error": apiErr.Public(reqID)}, sseTerminalWriteTimeout) {
		st.dead = true
			st.writeFailed()
	}
}

// sseWriteTimeout bounds one change-frame write: a subscriber that stops
// reading blocks the write on a full socket buffer indefinitely — no
// server WriteTimeout covers a long-lived stream (a blanket one would cut
// healthy idle streams), and a stuck write holding the stream mutex would
// wedge the terminal, the overflow close, and the handler (graceful
// shutdown included) behind it. A per-write deadline turns a stopped
// reader into an ordinary failed write: the stream dies and tears its
// session down. The deadline is CLEARED after the frame lands — a timer
// left armed fires on an idle stream (HTTP/2 resets it) and an expired one
// fails the next write even on HTTP/1. Package vars so tests can shrink
// them; ResponseWriters that cannot take a deadline (test recorders)
// ignore the attempt.
var sseWriteTimeout = 3 * time.Second

// sseTerminalWriteTimeout bounds the stream's last words — the close and
// error frames: a stalled change frame (up to sseWriteTimeout) can precede
// the terminal, and the two sequential writes together must stay inside
// main's five-second graceful-shutdown budget (3s + 1.5s < 5s).
var sseTerminalWriteTimeout = 1500 * time.Millisecond

// sseEvent frames one server-sent event — named event, single-line JSON
// data, blank-line terminator — and flushes it immediately: a stream frame
// held in a buffer is a frame the subscriber has not received. It reports
// false when the write failed (the subscriber disconnected), so the caller
// can stop instead of spinning on a dead connection.
func sseEvent(w http.ResponseWriter, event string, data any) bool {
	return sseEventBounded(w, event, data, sseWriteTimeout)
}

// sseEventBounded is sseEvent with an explicit write bound — the terminal
// frames ride a tighter one (sseTerminalWriteTimeout) so a stalled change
// frame followed by the close frame stays inside main's shutdown budget.
func sseEventBounded(w http.ResponseWriter, event string, data any, timeout time.Duration) bool {
	payload, err := sseJSON(data)
	if err != nil {
		return false
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return false
	}
	if !sseFlush(w) {
		// The frame reached net/http's buffer but not the wire: the socket
		// write failed inside the flush — this IS a failed write.
		return false
	}
	// Clear the deadline once the frame lands: a live-armed timer fires on
	// an idle stream — HTTP/2 resets the stream outright — and a deadline
	// left armed past its expiry fails the NEXT write even on HTTP/1. The
	// bound covers only the in-flight write.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
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

// sseFlush pushes buffered frames to the wire and reports whether the
// push reached it: fmt.Fprintf can succeed into net/http's buffer while
// the actual socket write fails inside the flush — a stalled reader whose
// deadline expires THERE leaves Fprintf's error unseen, and a caller that
// ignored the flush failure would keep a dead stream "alive" forever.
// ResponseController.Flush surfaces the flush's own error; ResponseWriters
// that cannot flush (test recorders) report success.
func sseFlush(w http.ResponseWriter) bool {
	return http.NewResponseController(w).Flush() == nil
}
