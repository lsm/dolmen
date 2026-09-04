package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/embed"
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
	// ErrCodeEmbedderUnavailable means the server's embedding provider could
	// not load its model — typically the local provider's first-use download
	// failing — so vectorized writes and text searches cannot run. The message
	// names the operator remediation; it is not the client's request that is
	// wrong, nor an unexpected server bug.
	ErrCodeEmbedderUnavailable ErrorCode = "embedder_unavailable"
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

// RequestIDFor returns the request id for r: the client's X-Request-Id when
// present, otherwise a freshly generated one. Both transports assign it
// before dispatch so every error envelope, log line, and response header
// carries an id — a failure the client did not tag can still be found in the
// server logs.
func RequestIDFor(r *http.Request) string {
	if id := requestIDFromHeader(r); id != "" {
		return id
	}
	return newRequestID()
}

// newRequestID mints a request id: 16 random bytes, hex-encoded. crypto/rand
// failing means the system entropy source is broken; a timestamp keeps ids
// unique even then rather than dropping the correlation entirely.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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
	// "provider|<url>|<model>" strings. Path components may contain spaces
	// ("C:\Users\Jane Doe\models"), so a component continues across
	// space-separated words; when a path is followed by prose the extra words
	// are swallowed into the redaction — over-redaction is safe, a leaked
	// fragment is not.
	filePathRe = regexp.MustCompile(`(^|[^A-Za-z0-9:|\\/.\-_])(?:[A-Za-z]:)?(?:[\\/]+[A-Za-z0-9_.\-]+(?:[ \t]+[A-Za-z0-9_.\-]+)*)+[\\/]*`)
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
	return strings.TrimSpace(redactPaths(msg))
}

// redactPaths replaces absolute file paths with <path>, keeping provider and
// model identifiers (which never start at a path separator) intact.
func redactPaths(msg string) string {
	return filePathRe.ReplaceAllString(msg, "${1}<path>")
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
	// A local model that cannot load (most often its first-use download
	// failing) is an operator-actionable condition, not an unexpected bug:
	// classify it and hand the client the offline remediations instead of the
	// sanitized nothing of internal_error.
	var le *embed.LoadError
	if errors.As(err, &le) {
		msg := redactStoreMsg(fmt.Sprintf(
			"embedding is unavailable: the local embedding model %s could not be loaded (%s); the first use of a local model downloads it from the Hugging Face Hub into the model cache — pre-seed the model cache per the README's local provider notes, or point DOLMEN_EMBED_MODEL at an absolute model-directory path, to serve without network access",
			le.Model, le.Err))
		return &Error{Status: http.StatusServiceUnavailable, Code: ErrCodeEmbedderUnavailable, Message: msg, Cause: err}
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
