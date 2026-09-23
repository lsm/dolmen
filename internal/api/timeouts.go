package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

const (
	DefaultReadTimeout    = 2 * time.Minute
	DefaultWriteTimeout   = 2 * time.Minute
	DefaultIdleTimeout    = 2 * time.Minute
	DefaultMaxHeaderBytes = 1 << 20
	DefaultOpTimeout      = 2 * time.Minute

	readHeaderTimeout = 10 * time.Second
)

type Timeouts struct {
	Read           time.Duration
	Write          time.Duration
	Idle           time.Duration
	MaxHeaderBytes int
	Op             time.Duration
	Migrate        time.Duration
}

func DefaultTimeouts() Timeouts {
	return Timeouts{
		Read:           DefaultReadTimeout,
		Write:          DefaultWriteTimeout,
		Idle:           DefaultIdleTimeout,
		MaxHeaderBytes: DefaultMaxHeaderBytes,
		Op:             DefaultOpTimeout,
	}
}

func (t Timeouts) Configure(srv *http.Server) {
	srv.ReadHeaderTimeout = readHeaderTimeout
	srv.ReadTimeout = t.Read
	srv.WriteTimeout = t.Write
	srv.IdleTimeout = t.Idle
	if t.Idle == 0 {
		srv.IdleTimeout = -1
	}
	srv.MaxHeaderBytes = t.MaxHeaderBytes
}

func WithTimeouts(t Timeouts) Option {
	return func(s *Server) {
		s.timeouts = t
	}
}

func (s *Server) ArmResponseWrite(w http.ResponseWriter) {
	var at time.Time
	if s.timeouts.Write > 0 {
		at = time.Now().Add(s.timeouts.Write)
	}
	_ = http.NewResponseController(w).SetWriteDeadline(at)
}

func (s *Server) opLimit(op string) (time.Duration, string) {
	if op == "migrate" {
		return s.timeouts.Migrate, fmt.Sprintf("%s migration time limit (-migrate-timeout, DOLMEN_MIGRATE_TIMEOUT)", s.timeouts.Migrate)
	}
	if s.timeouts.Op <= 0 {
		return 0, ""
	}
	if op == "wait_for" {
		return s.timeouts.Op + maxWaitForTimeoutMS*time.Millisecond,
			fmt.Sprintf("%s operation time limit (-op-timeout, DOLMEN_OP_TIMEOUT) beyond its timeout_ms", s.timeouts.Op)
	}
	return s.timeouts.Op, fmt.Sprintf("%s operation time limit (-op-timeout, DOLMEN_OP_TIMEOUT)", s.timeouts.Op)
}

func (s *Server) withOpDeadline(ctx context.Context, op string) (context.Context, context.CancelFunc, func(error) error) {
	limit, described := s.opLimit(op)
	if limit <= 0 {
		return ctx, func() {}, func(err error) error { return err }
	}
	opCtx, cancel := context.WithTimeout(ctx, limit)
	return opCtx, cancel, func(err error) error {
		if err == nil || ctx.Err() != nil || !errors.Is(opCtx.Err(), context.DeadlineExceeded) {
			return err
		}
		return opTimedOut(op, described, err)
	}
}

func opTimedOut(op, limit string, cause error) *Error {
	msg := fmt.Sprintf("%s did not finish within the server's %s and was stopped; a read can be retried or narrowed (a filter, a smaller limit), and a write may or may not have committed, so check with a query before retrying it", op, limit)
	if op == "migrate" {
		msg = fmt.Sprintf("migrate did not finish within the server's %s and was stopped; check the table's version with describe_table to see whether it applied before retrying, and ask the operator to raise the limit for a migration this large", limit)
	}
	return &Error{Status: http.StatusGatewayTimeout, Code: ErrCodeTimeout, Message: msg, Cause: cause}
}

func timedOut(err error) *Error {
	return &Error{Status: http.StatusGatewayTimeout, Code: ErrCodeTimeout, Message: "the operation ran out of time before it completed and was stopped; a read can be retried or narrowed, and a write may or may not have committed, so check with a query before retrying it", Cause: err}
}

func bodyReadTimedOut(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}

func (s *Server) BodyReadTimeout(err error) (string, bool) {
	if !bodyReadTimedOut(err) {
		return "", false
	}
	return s.bodyTimeoutError().Message, true
}

func (s *Server) bodyTimeoutError() *Error {
	return &Error{Status: http.StatusRequestTimeout, Code: ErrCodeTimeout, Message: fmt.Sprintf("the request body did not arrive within the server's %s read limit (-read-timeout, DOLMEN_READ_TIMEOUT); send a smaller request, or send it faster", s.timeouts.Read)}
}
