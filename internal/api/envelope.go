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

type ErrorCode string

const (
	ErrCodeInvalid ErrorCode = "invalid_request"

	ErrCodeNotFound ErrorCode = "not_found"

	ErrCodeQuery ErrorCode = "query_error"

	ErrCodeConflict ErrorCode = "conflict"

	ErrCodeForbidden ErrorCode = "forbidden"

	ErrCodeEmbedderUnavailable ErrorCode = "embedder_unavailable"

	ErrCodeInternal ErrorCode = "internal_error"
)

type Error struct {
	Status  int
	Code    ErrorCode
	Message string
	Cause   error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

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

func RequestIDFor(r *http.Request) string {
	if id := requestIDFromHeader(r); id != "" {
		return id
	}
	return NewRequestID()
}

func NewRequestID() string {
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
	filePathRe = regexp.MustCompile(`(^|[^A-Za-z0-9:|\\/.\-_])(?:[A-Za-z]:)?(?:[\\/]+[A-Za-z0-9_.\-]+(?:[ \t]+[A-Za-z0-9_.\-]+)*)+[\\/]*`)
)

func redactStoreMsg(msg string) string {
	if msg == "" {
		return "invalid request"
	}
	return strings.TrimSpace(redactPaths(msg))
}

func redactPaths(msg string) string {
	return filePathRe.ReplaceAllString(msg, "${1}<path>")
}

func isConflict(msg string) bool {
	return strings.Contains(msg, "idempotency key") && strings.Contains(msg, "different") ||
		strings.Contains(msg, "matches multiple")
}

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
		cause := err
		var r *store.RedactedSQLite
		if errors.As(err, &r) {
			cause = r.Cause()
		}
		return &Error{Status: http.StatusBadRequest, Code: code, Message: msg, Cause: cause}
	}

	var le *embed.LoadError
	if errors.As(err, &le) {
		msg := redactStoreMsg(fmt.Sprintf(
			"embedding is unavailable: the local embedding model %s could not be loaded (first use downloads it from the Hugging Face Hub into the server's model cache); the failure is often transient — retry the request, which retries the download (a failed write rolls back and consumes no idempotency key); to serve without network access, pre-seed the model cache by placing the model's files in it as %s (the org--name form of the model id), or point DOLMEN_EMBED_MODEL at an absolute model-directory path; the underlying cause is in the server log under this request id",
			le.Model, le.CacheDirName()))
		if !le.IsHubID() {
			msg = redactStoreMsg("embedding is unavailable: the configured local model directory (DOLMEN_EMBED_MODEL) could not be loaded; retry the request in case the failure was transient (a failed write rolls back and consumes no idempotency key), and check that the directory holds a complete model (config, tokenizer, and weight files) or point DOLMEN_EMBED_MODEL at one that does; the underlying cause is in the server log under this request id")
		}
		return &Error{Status: http.StatusServiceUnavailable, Code: ErrCodeEmbedderUnavailable, Message: msg, Cause: err}
	}
	return &Error{Status: http.StatusInternalServerError, Code: ErrCodeInternal, Message: "internal error", Cause: err}
}

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
