package sqliteindex

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	gb "github.com/lsm/dolmen/spike/graph-index"
	_ "modernc.org/sqlite"
)

type index struct{ db *sql.DB }
type view struct{ tx *sql.Tx }

func Open(dir string) (gb.Index, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, "graph.db")}
	params := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(FULL)", "cache_size(-32768)", "mmap_size(0)", "busy_timeout(5000)"} {
		params.Add("_pragma", p)
	}
	u.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS edges(id INTEGER PRIMARY KEY,src INTEGER NOT NULL,dst INTEGER NOT NULL,kind INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS outgoing ON edges(src,dst,id);
 CREATE INDEX IF NOT EXISTS incoming ON edges(dst,src,id);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &index{db}, nil
}

func (i *index) Append(ctx context.Context, edges []gb.Edge) error {
	if err := gb.Validate(edges); err != nil {
		return err
	}
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO edges VALUES(?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range edges {
		if _, err := stmt.ExecContext(ctx, e.ID, e.From, e.To, e.Kind); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (i *index) Snapshot(ctx context.Context) (gb.View, error) {
	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	var id uint64
	err = tx.QueryRowContext(ctx, "SELECT id FROM edges LIMIT 1").Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		tx.Rollback()
		return nil, err
	}
	return &view{tx}, nil
}

func (v *view) Adjacent(ctx context.Context, node uint64, reverse bool, limit int) ([]gb.Edge, bool, error) {
	if limit < 0 || limit > 1000000 {
		return nil, false, fmt.Errorf("adjacency limit must be between zero and one million")
	}
	query := "SELECT id,src,dst,kind FROM edges WHERE src=? ORDER BY dst,id LIMIT ?"
	if reverse {
		query = "SELECT id,src,dst,kind FROM edges WHERE dst=? ORDER BY src,id LIMIT ?"
	}
	rows, err := v.tx.QueryContext(ctx, query, node, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	edges := []gb.Edge{}
	for rows.Next() {
		var e gb.Edge
		if err := rows.Scan(&e.ID, &e.From, &e.To, &e.Kind); err != nil {
			return nil, false, err
		}
		if len(edges) == limit {
			return edges, true, nil
		}
		edges = append(edges, e)
	}
	return edges, false, rows.Err()
}
func (v *view) Close() error  { return v.tx.Rollback() }
func (i *index) Close() error { return i.db.Close() }
