package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/telemetry/dbspan"
)

func newTracer(tp trace.TracerProvider, pool *pgxpool.Pool) *dbspan.Tracer {
	base := []attribute.KeyValue{semconv.DBSystemNamePostgreSQL}
	if pool != nil {
		if cc := pool.Config().ConnConfig; cc != nil {
			if cc.Host != "" {
				base = append(base, semconv.ServerAddress(dbspan.Clean(cc.Host, dbspan.MaxNameAttr)))
			}
			if cc.Port != 0 {
				base = append(base, semconv.ServerPort(int(cc.Port)))
			}
		}
	}
	return dbspan.New(tp, store.SpanErrorType, trace.SpanKindClient, base...)
}

func (s *Store) span(ctx context.Context, op, ns, table string, extra ...attribute.KeyValue) (context.Context, func(error)) {
	ctx, sp := s.tr.Op(ctx, op, ns, table, extra...)
	return ctx, func(err error) { s.tr.End(sp, err) }
}
