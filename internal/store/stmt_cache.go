package store

import (
	"context"
	"database/sql"
)

type stmtExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type stmtCache struct {
	tx    *sql.Tx
	stmts map[string]*sql.Stmt
}

func newStmtCache(tx *sql.Tx) *stmtCache {
	return &stmtCache{tx: tx, stmts: map[string]*sql.Stmt{}}
}

func (c *stmtCache) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	st, ok := c.stmts[query]
	if !ok {
		var err error
		if st, err = c.tx.PrepareContext(ctx, query); err != nil {
			return nil, err
		}
		c.stmts[query] = st
	}
	return st.ExecContext(ctx, args...)
}

func (c *stmtCache) close() {
	for _, st := range c.stmts {
		st.Close()
	}
}
