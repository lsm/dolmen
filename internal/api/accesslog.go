package api

import (
	"context"
	"log/slog"
	"time"
)

func logOp(ctx context.Context, op string, bodyBytes int, start time.Time, err error) {
	if !slog.Default().Enabled(ctx, slog.LevelDebug) {
		return
	}
	outcome := "ok"
	status := 200
	if err != nil {
		e := WrapError(err)
		outcome, status = string(e.Code), e.Status
	}
	slog.Debug("op", "op", op, "outcome", outcome, "status", status, "duration_ms", time.Since(start).Milliseconds(), "request_bytes", bodyBytes, "request_id", RequestIDFrom(ctx))
}
