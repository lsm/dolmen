package store

import (
	"context"
	"database/sql"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const MaxValueBytes = 2 * MaxQueryBytes

func beginCallerTx(ctx context.Context, db *sql.DB, opts *sql.TxOptions) (*sql.Tx, func(), error) {
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := sqlite.Limit(c, sqlite3.SQLITE_LIMIT_LENGTH, MaxValueBytes); err != nil {
		c.Close()
		return nil, nil, err
	}
	tx, err := c.BeginTx(ctx, opts)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	return tx, func() {
		tx.Rollback()
		c.Close()
	}, nil
}
