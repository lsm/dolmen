package store

import (
	"context"
	"database/sql"
	"errors"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/telemetry/dbspan"
)

func WithTracerProvider(tp trace.TracerProvider) OpenOption {
	return func(s *Store) { s.tp = tp }
}

func newSQLiteTracer(tp trace.TracerProvider) *dbspan.Tracer {
	return dbspan.New(tp, SpanErrorType, trace.SpanKindInternal, semconv.DBSystemNameSQLite)
}

func SpanErrorType(err error) string {
	var de *derr.Error
	var qe *QueryError
	var vce *VersionConflictError
	switch {
	case errors.As(err, &de):
		return string(de.Code)
	case errors.Is(err, context.Canceled):
		return string(derr.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return string(derr.Timeout)
	case errors.Is(err, ErrNotFound):
		return string(derr.NotFound)
	case errors.As(err, &qe):
		return string(derr.Query)
	case errors.As(err, &vce), errors.Is(err, derr.ErrConflict):
		return string(derr.Conflict)
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrCatalogTooNew):
		return string(derr.InvalidRequest)
	}
	return string(derr.Internal)
}

type writeSpan struct {
	span      trace.Span
	committed bool
}

func (w *writeSpan) commit() {
	if w != nil {
		w.committed = true
	}
}

func (s *Store) beginWrite(ctx context.Context, n *nsDB) (context.Context, *sql.Tx, *writeSpan, error) {
	if !s.tr.On() {
		tx, err := n.rw.BeginTx(ctx, nil)
		return ctx, tx, nil, err
	}
	wctx, wait := s.tr.Child(ctx, dbspan.WriterWait)
	tx, err := n.rw.BeginTx(wctx, nil)
	s.tr.End(wait, err)
	if err != nil {
		return ctx, nil, nil, err
	}
	ctx, span := s.tr.Child(ctx, dbspan.Transaction)
	return ctx, tx, &writeSpan{span: span}, nil
}

func (s *Store) endWrite(tx *sql.Tx, w *writeSpan) {
	tx.Rollback()
	if w == nil {
		return
	}
	outcome := "rollback"
	if w.committed {
		outcome = "commit"
	}
	w.span.SetAttributes(dbspan.TxOutcomeKey.String(outcome))
	w.span.End()
}

func (s *Store) writerConn(ctx context.Context, n *nsDB) (*sql.Conn, error) {
	if !s.tr.On() {
		return n.rw.Conn(ctx)
	}
	wctx, wait := s.tr.Child(ctx, dbspan.WriterWait)
	conn, err := n.rw.Conn(wctx)
	s.tr.End(wait, err)
	return conn, err
}

func (s *Store) migrateStepSpan(ctx context.Context, kind string, index int) (context.Context, func(error)) {
	if !s.tr.On() {
		return ctx, func(error) {}
	}
	ctx, span := s.tr.Child(ctx, dbspan.MigrateStep, dbspan.MigrateStepKindKey.String(kind), dbspan.MigrateStepIndexKey.Int(index))
	ended := false
	return ctx, func(err error) {
		if !ended {
			ended = true
			s.tr.End(span, err)
		}
	}
}

func (s *Store) migrateStep(ctx context.Context, kind string, index int, step func(context.Context) error) error {
	ctx, end := s.migrateStepSpan(ctx, kind, index)
	err := step(ctx)
	end(err)
	return err
}

func (c *vecCache) endCache(span trace.Span, outcome string, err error) {
	if !c.tr.On() {
		return
	}
	span.SetAttributes(dbspan.VectorCacheKey.String(outcome))
	c.tr.End(span, err)
}

func (c *vecCache) endScore(span trace.Span, scored, candidates int, err error) {
	if !c.tr.On() {
		return
	}
	span.SetAttributes(dbspan.VectorRowsScoredKey.Int(scored))
	if candidates >= 0 {
		span.SetAttributes(dbspan.VectorCandidatesKey.Int(candidates))
	}
	c.tr.End(span, err)
}

func (s *Store) writerTableGen(ctx context.Context, n *nsDB, table string) (int64, error) {
	if !s.tr.On() {
		return tableGen(ctx, n.rw, table)
	}
	conn, err := s.writerConn(ctx, n)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return tableGen(ctx, conn, table)
}

func commitWrite(tx *sql.Tx, w *writeSpan) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	w.commit()
	return nil
}
