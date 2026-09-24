package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) keepUnknownKeys(ctx context.Context, tx pgx.Tx, ns, table string, next []byte, renames map[string]string) ([]byte, error) {
	var prev string
	err := tx.QueryRow(ctx, "SELECT schema_json FROM "+s.relation("tables")+" WHERE namespace=$1 AND name=$2", ns, table).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return next, nil
	}
	if err != nil {
		return nil, err
	}
	merged, err := store.MergeUnknownSchemaKeys(prev, next, renames)
	return []byte(merged), err
}
