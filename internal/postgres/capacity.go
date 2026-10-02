package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lsm/dolmen/internal/derr"
)

func connCapacity(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "53300" || pgErr.Code == "57P03") {
		return &derr.Error{Code: derr.Timeout, Message: "the database has no free connection right now (PostgreSQL is at its connection limit); retry shortly, or reduce concurrent subscriptions and requests", Cause: err}
	}
	return err
}

func (s *Store) beginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, connCapacity(err)
	}
	return tx, nil
}
