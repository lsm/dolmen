package postgres

import (
	"context"
	"fmt"
	"log/slog"
)

type sqlWork struct{ slots chan struct{} }

func newSQLWork(limit int) *sqlWork { return &sqlWork{slots: make(chan struct{}, limit)} }

var compilerWork = newSQLWork(4)

func (w *sqlWork) run(ctx context.Context, work func() (string, *sqlNames, error)) (string, *sqlNames, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	select {
	case w.slots <- struct{}{}:
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
	type result struct {
		sql   string
		names *sqlNames
		err   error
	}
	done := make(chan result, 1)
	go func() {
		out := result{}
		defer func() {
			if p := recover(); p != nil {
				slog.Error("PostgreSQL SQL compiler panicked", "panic", p)
				out.err = fmt.Errorf("PostgreSQL SQL compiler failed")
			}
			<-w.slots
			done <- out
		}()
		if err := ctx.Err(); err != nil {
			out.err = err
			return
		}
		out.sql, out.names, out.err = work()
	}()
	select {
	case out := <-done:
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		return out.sql, out.names, out.err
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
}
