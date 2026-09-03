package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/lsm/dolmen/internal/store"
)

// ErrorCode is a stable, documented machine-readable string identifying an
// error class. Clients should branch on these values; messages are for humans.
type ErrorCode string

const (
	// ErrCodeInvalid is the default for malformed or disallowed request data.
	ErrCodeInvalid ErrorCode = "invalid_request"
	// ErrCodeNotFound means the requested namespace or table does not exist.
	ErrCodeNotFound ErrorCode = "not_found"
	// ErrCodeQuery means a SQL query or filter could not be executed.
	ErrCodeQuery ErrorCode = "query_error"
	// ErrCodeConflict means the request collides with existing state (e.g.
	// an idempotency key reused with different records, or a non-unique key).
	ErrCodeConflict ErrorCode = "conflict"
	// ErrCodeForbidden means the request is not allowed (e.g. disallowed origin).
	ErrCodeForbidden ErrorCode = "forbidden"
	// ErrCodeInternal means an unexpected server-side problem occurred.
	ErrCodeInternal ErrorCode = "internal_error"
)

// Error is an API-facing error. It carries a stable code, an HTTP status, and a
// sanitized message. The optional Cause is logged by writeError but is never
// serialized to the client.
type Error struct {
	Status  int
	Code    ErrorCode
	Message string
	Cause   error
}

func (e *Error) Error() string { return e.Message }

// Unwrap returns the underlying cause for logging and tests.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Public returns the client-safe error envelope, optionally including a
// request ID. It is the shape shared by the HTTP and MCP surfaces.
func (e *Error) Public(requestID string) map[string]any {
	m := map[string]any{
		"code":    e.Code,
		"message": e.Message,
	}
	if requestID != "" {
		m["request_id"] = requestID
	}
	return m
}

// requestIDKey is the context key for an echoed request ID.
type requestIDKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func requestIDFromHeader(r *http.Request) string {
	return r.Header.Get("X-Request-Id")
}

func badRequest(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: ErrCodeInvalid, Message: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) *Error {
	return &Error{Status: http.StatusNotFound, Code: ErrCodeNotFound, Message: fmt.Sprintf(format, args...)}
}

func internal(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: ErrCodeInternal, Message: "internal error", Cause: err}
}

func forbidden(format string, args ...any) *Error {
	return &Error{Status: http.StatusForbidden, Code: ErrCodeForbidden, Message: fmt.Sprintf(format, args...)}
}

var (
	// filePathRe matches Unix/Windows-style absolute paths that may leak in
	// internal errors. It preserves the leading separator so it does not match
	// URL paths (://) or slash-bearing provider/model identifiers in
	// "provider|<url>|<model>" strings.
	filePathRe = regexp.MustCompile(`(^|[^A-Za-z0-9:|\\/.\-_])(?:[A-Za-z]:)?(?:[\\/]+[A-Za-z0-9_.\-]+)+[\\/]*`)
)

// redactStoreMsg removes internal paths and raw SQLite internals from a store
// error message while preserving the hand-written, client-helpful strings.
func redactStoreMsg(msg string) string {
	if msg == "" {
		return "invalid request"
	}
	if strings.Contains(msg, "SQL logic error:") ||
		strings.Contains(msg, "SQLITE_") ||
		strings.Contains(msg, "misuse at line") {
		return store.RedactSQLMessage(msg)
	}
	msg = filePathRe.ReplaceAllString(msg, "${1}<path>")
	return strings.TrimSpace(msg)
}

// isConflict reports whether a store error message describes a state conflict.
func isConflict(msg string) bool {
	return strings.Contains(msg, "idempotency key") && strings.Contains(msg, "different") ||
		strings.Contains(msg, "matches multiple")
}

// wrapStoreErr maps store errors to stable API errors. It redacts raw SQLite
// internals and internal file paths from client-facing messages.
func wrapStoreErr(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	var qe *store.QueryError
	if errors.As(err, &qe) {
		code := ErrCodeQuery
		status := http.StatusBadRequest
		if errors.Is(qe, store.ErrNotFound) {
			code = ErrCodeNotFound
			status = http.StatusNotFound
		}
		return &Error{Status: status, Code: code, Message: qe.Error(), Cause: qe.Cause()}
	}
	if errors.Is(err, store.ErrNotFound) {
		msg := redactStoreMsg(err.Error())
		msg = strings.TrimPrefix(msg, store.ErrNotFound.Error()+": ")
		return &Error{Status: http.StatusNotFound, Code: ErrCodeNotFound, Message: msg, Cause: err}
	}
	var vce *store.VersionConflictError
	if errors.As(err, &vce) {
		return &Error{Status: http.StatusConflict, Code: ErrCodeConflict, Message: err.Error(), Cause: err}
	}
	if errors.Is(err, store.ErrInvalid) {
		msg := redactStoreMsg(err.Error())
		msg = strings.TrimPrefix(msg, store.ErrInvalid.Error()+": ")
		code := ErrCodeInvalid
		if isConflict(err.Error()) {
			code = ErrCodeConflict
		}
		return &Error{Status: http.StatusBadRequest, Code: code, Message: msg, Cause: err}
	}
	return &Error{Status: http.StatusInternalServerError, Code: ErrCodeInternal, Message: "internal error", Cause: err}
}

// WrapError returns err as an API Error, wrapping unknown errors as internal.
func WrapError(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return internal(err)
}
