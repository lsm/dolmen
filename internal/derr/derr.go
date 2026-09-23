package derr

import "fmt"

type Code string

const (
	InvalidRequest      Code = "invalid_request"
	NotFound            Code = "not_found"
	Query               Code = "query_error"
	Conflict            Code = "conflict"
	Unauthorized        Code = "unauthorized"
	Forbidden           Code = "forbidden"
	EmbedderUnavailable Code = "embedder_unavailable"
	Canceled            Code = "canceled"
	Timeout             Code = "timeout"
	Internal            Code = "internal_error"
)

type Error struct {
	Code    Code
	Message string
	Cause   error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code Code, cause error) *Error {
	if cause == nil {
		return &Error{Code: code, Message: string(code)}
	}
	return &Error{Code: code, Message: cause.Error(), Cause: cause}
}

var (
	ErrInvalidRequest      = &Error{Code: InvalidRequest}
	ErrNotFound            = &Error{Code: NotFound}
	ErrQuery               = &Error{Code: Query}
	ErrConflict            = &Error{Code: Conflict}
	ErrUnauthorized        = &Error{Code: Unauthorized}
	ErrForbidden           = &Error{Code: Forbidden}
	ErrEmbedderUnavailable = &Error{Code: EmbedderUnavailable}
	ErrCanceled            = &Error{Code: Canceled}
	ErrTimeout             = &Error{Code: Timeout}
	ErrInternal            = &Error{Code: Internal}
)
