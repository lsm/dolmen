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

const sseWriteDeadline = 10 * time.Second

const defaultKeepaliveInterval = 20 * time.Second

func (s *Server) HandleSubscribe(w http.ResponseWriter, r *http.Request) {

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

	table := ""
	if q.Has("table") {
		table = normTable(q.Get("table"))
		if table == "" {
			writeError(w, r, badRequest("table must be a non-empty table name — omit the parameter for the namespace-wide feed"))
			return
		}
	}
	if err := s.authorizeFeed(r.Context(), q.Get("namespace"), table); err != nil {
		writeError(w, r, WrapError(err))
		return
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
	ctx, stopCause := context.WithCancelCause(r.Context())
	stop := func() { stopCause(nil) }
	defer stop()
	go func() {
		select {
		case <-s.drainCh():
			stopCause(errDraining)
		case <-ctx.Done():
		}
	}()
	closing := func() bool {
		c := context.Cause(ctx)
		return errors.Is(c, store.ErrListenAged) || errors.Is(c, errDraining)
	}
	if s.maxSubscriptionAge > 0 {
		var ageStop context.CancelFunc
		ctx, ageStop = context.WithTimeoutCause(ctx, s.maxSubscriptionAge, store.ErrListenAged)
		defer ageStop()
	}
	live := make(chan store.ChangeRecord, 1)
	ended := make(chan error, 1)
	replay, cancel, err := s.eng.Listen(ctx, ns, table, cursor, [16]byte{}, s.liveAuthz(r, ns),
		func(rec store.ChangeRecord) {
			select {
			case live <- rec:
			case <-ctx.Done():
			}
		},
		func(cause error) {
			select {
			case ended <- cause:
			case <-r.Context().Done():
			}
		})

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
	closeWith := func(apiErr *Error) {
		for {
			select {
			case rec := <-live:
				if !write(rec) {
					return
				}
			default:
				sseEvent(w, "close", sseCursor{Cursor: string(resume)})
				sseErrorEvent(w, apiErr, reqID)
				return
			}
		}
	}

	for {
		records, _, done, nerr := replay.Next(ctx)
		if nerr != nil {
			if ctx.Err() != nil && !closing() {
				return
			}

			select {
			case cause := <-ended:
				closeWith(subscribeErr(cause))
			case <-r.Context().Done():
				return
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
		if s.holdReplay != nil {
			s.holdReplay()
		}
	}
	if s.holdReplay != nil {
		s.holdReplay()
	}

	if !sseEvent(w, "ready", sseCursor{Cursor: string(resume)}) {
		return
	}

	var keepalive <-chan time.Time
	if s.keepaliveInterval > 0 {
		ticker := time.NewTicker(s.keepaliveInterval)
		defer ticker.Stop()
		keepalive = ticker.C
	}
	for {
		select {
		case rec := <-live:
			if !write(rec) {
				return
			}
		case cause := <-ended:
			closeWith(subscribeErr(cause))
			return
		case <-keepalive:
			if !sseComment(w, ": keepalive") {
				return
			}
		case <-ctx.Done():
			if !closing() {
				return
			}
			closeWith(subscribeErr(context.Cause(ctx)))
			return
		}
	}
}

func sseComment(w http.ResponseWriter, text string) bool {
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
	_, werr := fmt.Fprintf(w, "%s\n\n", text)
	ferr := rc.Flush()
	rc.SetWriteDeadline(time.Time{})
	return werr == nil && ferr == nil
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
	case errors.Is(err, errDraining):
		return badRequest("the server is shutting down; reconnect from the cursor in the preceding close frame to resume exactly where this stream ended, on a server that is still serving")
	case errors.Is(err, store.ErrListenAged):
		return badRequest("subscription reached the maximum subscription age (-max-subscription-age, default 30m); reconnect from the cursor in the preceding close frame to resume exactly where this stream ended — the fresh connection re-asserts your credentials")
	default:
		return wrapStoreErr(err)
	}
}

type sseChange struct {
	Cursor string `json:"cursor"`
	Table  string `json:"table"`
	RowID  int64  `json:"row_id"`
	Kind   string `json:"kind"`
}

type sseCursor struct {
	Cursor string `json:"cursor"`
}

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

func sseErrorEvent(w http.ResponseWriter, apiErr *Error, reqID string) {
	status := apiErr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}

	if status >= http.StatusInternalServerError {
		slog.Error("sse error", "code", apiErr.Code, "status", status, "request_id", reqID, "cause", apiErr.Cause)
	} else {
		slog.Debug("sse error", "code", apiErr.Code, "status", status, "request_id", reqID, "cause", apiErr.Cause)
	}
	sseEvent(w, "error", map[string]any{"ok": false, "error": apiErr.Public(reqID)})
}

func sseJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
