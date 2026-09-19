package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/ops"
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

	ErrCodeCanceled ErrorCode = "canceled"

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

type unknownFieldError struct {
	Field string
}

func (e *unknownFieldError) Error() string {
	return "unknown field " + strconv.Quote(e.Field)
}

func unknownJSONField(err error) (string, bool) {
	quoted, ok := strings.CutPrefix(err.Error(), "json: unknown field ")
	if !ok {
		return "", false
	}
	field, unquoteErr := strconv.Unquote(quoted)
	if unquoteErr != nil {
		return "", false
	}
	return field, true
}

type typeMismatchError struct {
	Field string
	Want  string
	Got   string
}

func (e *typeMismatchError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("request body must be a JSON object, but the request sent %s", e.Got)
	}
	return fmt.Sprintf("field %q must be %s, but the request sent %s", e.Field, e.Want, e.Got)
}

func jsonShapeOf(t reflect.Type) string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && t.Kind() == reflect.Interface {
		return "a JSON value"
	}
	if t == nil {
		return "a different type"
	}
	if t == reflect.TypeOf(json.Number("")) || t == reflect.TypeOf(json.RawMessage(nil)) {
		if t == reflect.TypeOf(json.Number("")) {
			return "a number"
		}
		return "a JSON value"
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Map, reflect.Struct:
		return "an object"
	}
	return "a different type"
}

func jsonValueWord(value string) string {
	switch value {
	case "number":
		return "a number"
	case "string":
		return "a string"
	case "bool", "true", "false":
		return "a boolean"
	case "array":
		return "an array"
	case "object":
		return "an object"
	case "null":
		return "null"
	}
	if strings.HasPrefix(value, "number ") {
		return "a number"
	}
	return "a different type"
}

func asTypeMismatch(err error) (*typeMismatchError, bool) {
	var ute *json.UnmarshalTypeError
	if !errors.As(err, &ute) {
		return nil, false
	}
	return &typeMismatchError{
		Field: ute.Field,
		Want:  jsonShapeOf(ute.Type),
		Got:   jsonValueWord(ute.Value),
	}, true
}

func frameTypeMismatch(err error, op string) *Error {
	var tm *typeMismatchError
	var apiErr *Error
	if !errors.As(err, &tm) || !errors.As(err, &apiErr) {
		return nil
	}
	if tm.Field == "" {
		return nil
	}
	reframed := *apiErr
	reframed.Message = fmt.Sprintf(
		"%s; see %s's InputSchema (MCP tools/list or /v1/openapi.json) for the accepted types",
		tm.Error(), op)
	return &reframed
}

func frameUnknownField(err error, op string) *Error {
	var uf *unknownFieldError
	var apiErr *Error
	if !errors.As(err, &uf) || !errors.As(err, &apiErr) {
		return nil
	}
	reframed := *apiErr
	reframed.Message = fmt.Sprintf(
		"unknown field %q on operation %s; see %s's InputSchema (MCP tools/list or /v1/openapi.json) for the accepted fields",
		uf.Field, op, op)
	return &reframed
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

func wrapStoreErr(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	var cve *store.CatalogVersionError
	if errors.As(err, &cve) {
		return &Error{Status: http.StatusBadRequest, Code: ErrCodeInvalid, Message: cve.Error(), Cause: err}
	}
	if errors.Is(err, store.ErrCatalogCorrupt) {
		return &Error{Status: http.StatusBadRequest, Code: ErrCodeInvalid, Message: redactStoreMsg(err.Error()), Cause: err}
	}
	var qe *store.QueryError
	if errors.As(err, &qe) {
		_, code := statusFor(ops.Classify(qe))
		status := http.StatusBadRequest
		if code == ErrCodeNotFound {
			status = http.StatusNotFound
		}
		return &Error{Status: status, Code: code, Message: qe.Error(), Cause: qe.Cause()}
	}
	if errors.Is(err, store.ErrNotFound) {
		msg := redactStoreMsg(err.Error())
		msg = strings.TrimPrefix(msg, store.ErrNotFound.Error()+": ")
		_, code := statusFor(ops.Classify(err))
		return &Error{Status: http.StatusNotFound, Code: code, Message: msg, Cause: err}
	}
	var vce *store.VersionConflictError
	if errors.As(err, &vce) {
		_, code := statusFor(ops.Classify(err))
		return &Error{Status: http.StatusConflict, Code: code, Message: err.Error(), Cause: err}
	}
	if errors.Is(err, store.ErrInvalid) {
		msg := redactStoreMsg(err.Error())
		msg = strings.TrimPrefix(msg, store.ErrInvalid.Error()+": ")
		_, code := statusFor(ops.Classify(err))
		cause := err
		var r *store.RedactedSQLite
		if errors.As(err, &r) {
			cause = r.Cause()
		}
		return &Error{Status: http.StatusBadRequest, Code: code, Message: msg, Cause: cause}
	}
	var shared *derr.Error
	if errors.As(err, &shared) {
		status, code := statusFor(shared.Code)
		msg := shared.Message
		if status == http.StatusInternalServerError {
			msg = "internal error"
		}
		return &Error{Status: status, Code: code, Message: msg, Cause: err}
	}

	var le *embed.LoadError
	if errors.As(err, &le) {
		msg := redactStoreMsg(fmt.Sprintf(
			"embedding is unavailable: the local embedding model %s could not be loaded (first use downloads it from the Hugging Face Hub into the server's model cache); the failure is often transient — retry the request, which retries the download (a failed write rolls back and consumes no idempotency key); to serve without network access, pre-seed the model cache by placing the model's files in it as %s (the org--name form of the model id), or point DOLMEN_EMBED_MODEL at an absolute model-directory path; the underlying cause is in the server log under this request id",
			le.Model, le.CacheDirName()))
		if !le.IsHubID() {
			msg = redactStoreMsg("embedding is unavailable: the configured local model directory (DOLMEN_EMBED_MODEL) could not be loaded; retry the request in case the failure was transient (a failed write rolls back and consumes no idempotency key), and check that the directory holds a complete model (config, tokenizer, and weight files) or point DOLMEN_EMBED_MODEL at one that does; the underlying cause is in the server log under this request id")
		}
		_, code := statusFor(ops.Classify(err))
		return &Error{Status: http.StatusServiceUnavailable, Code: code, Message: msg, Cause: err}
	}
	return &Error{Status: http.StatusInternalServerError, Code: ErrCodeInternal, Message: "internal error", Cause: err}
}

func canceled(err error) *Error {
	return &Error{Status: http.StatusOK, Code: ErrCodeCanceled, Message: "the request was cancelled before it completed; the operation may or may not have finished server-side — check with a query before retrying a write", Cause: err}
}

func statusFor(code derr.Code) (int, ErrorCode) {
	switch code {
	case derr.InvalidRequest:
		return http.StatusBadRequest, ErrCodeInvalid
	case derr.NotFound:
		return http.StatusNotFound, ErrCodeNotFound
	case derr.Query:
		return http.StatusBadRequest, ErrCodeQuery
	case derr.Conflict:
		return http.StatusBadRequest, ErrCodeConflict
	case derr.Forbidden:
		return http.StatusForbidden, ErrCodeForbidden
	case derr.EmbedderUnavailable:
		return http.StatusServiceUnavailable, ErrCodeEmbedderUnavailable
	case derr.Canceled:
		return http.StatusOK, ErrCodeCanceled
	default:
		return http.StatusInternalServerError, ErrCodeInternal
	}
}

func WrapError(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		if apiErr.Code == ErrCodeInternal && errors.Is(apiErr, context.Canceled) {
			return canceled(err)
		}
		return apiErr
	}
	if errors.Is(err, context.Canceled) {
		return canceled(err)
	}
	return internal(err)
}
