package postgres

import (
	"context"
	"errors"
	"log/slog"
)

var (
	errNotReachable   = errors.New("the PostgreSQL database is unreachable, so no request can be served; the server log has the cause")
	errCatalogMissing = errors.New("the PostgreSQL catalog schema is missing; restart the server so it can recreate it")
)

func (s *Store) Ready(ctx context.Context) error {
	var present bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regnamespace($1) IS NOT NULL`, s.catalog).Scan(&present); err != nil {
		slog.Warn("readiness: PostgreSQL did not answer", "err", err)
		return errNotReachable
	}
	if !present {
		slog.Warn("readiness: the catalog schema is missing", "catalog", s.catalog)
		return errCatalogMissing
	}
	return nil
}
