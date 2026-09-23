package postgres

import (
	"context"
	"fmt"
)

func (s *Store) Ready(ctx context.Context) error {
	var present bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regnamespace($1) IS NOT NULL`, s.catalog).Scan(&present); err != nil {
		return fmt.Errorf("the PostgreSQL database is unreachable, so no request can be served: %w", err)
	}
	if !present {
		return fmt.Errorf("the PostgreSQL catalog schema %s is missing; restart the server so it can recreate it", s.catalog)
	}
	return nil
}
