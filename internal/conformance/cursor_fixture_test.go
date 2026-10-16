package conformance

import (
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/store"
)

func (h *harness) expireChangeCursor(ns, token string) {
	h.t.Helper()
	ctx := h.t.Context()
	if testEngine(h.t) == store.EnginePostgres {
		conn, err := pgx.Connect(ctx, postgresDSN(h.t))
		if err != nil {
			h.t.Fatal(err)
		}
		defer conn.Close(ctx)
		relation := pgx.Identifier{postgresCatalog(h.dir), "cursors"}.Sanitize()
		result, err := conn.Exec(ctx, "UPDATE "+relation+" SET issued_at=$1, chain_start=$1 WHERE namespace=$2 AND token=$3", time.Unix(0, 0).UTC(), ns, token)
		if err != nil || result.RowsAffected() != 1 {
			h.t.Fatalf("age PostgreSQL cursor: rows=%d err=%v", result.RowsAffected(), err)
		}
		return
	}
	path, relation := filepath.Join(h.dir, ns+".db"), "_dolmen_cursor_tokens"
	if testEngine(h.t) == store.EngineLakehouse {
		path, relation = filepath.Join(h.dir, ns+".lakehouse", "catalog.db"), "_dolmen_lakehouse_cursors"
	}
	db, err := openSQL(path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	result, err := db.Exec("UPDATE "+relation+" SET issued_at=0, chain_start=0 WHERE token=?", token)
	if err != nil {
		h.t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		h.t.Fatalf("age SQLite catalog cursor: rows=%d err=%v", rows, err)
	}
}
