package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

type cursorState struct {
	position int64
	origin   int64
	start    time.Time
	issued   time.Time
	table    string
	drop     int64
}

func (s *Store) mintCursor(ctx context.Context, tx pgx.Tx, n namespace, state cursorState, now time.Time) (store.Cursor, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	_, err := tx.Exec(ctx, "INSERT INTO "+s.relation("cursors")+" (namespace,token,position,chain_origin,chain_start,issued_at,table_name,drop_generation) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", n.name, token, state.position, state.origin, state.start, now, state.table, state.drop)
	return store.Cursor(token), err
}

func (s *Store) resolveCursor(ctx context.Context, tx pgx.Tx, n namespace, token store.Cursor, table string, drop int64, now time.Time) (cursorState, error) {
	var state cursorState
	err := tx.QueryRow(ctx, "SELECT position,chain_origin,chain_start,issued_at,table_name,drop_generation FROM "+s.relation("cursors")+" WHERE namespace=$1 AND token=$2", n.name, string(token)).Scan(&state.position, &state.origin, &state.start, &state.issued, &state.table, &state.drop)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, store.ErrCursorExpired
	}
	if err != nil {
		return state, err
	}
	if state.table != table {
		return state, store.ErrCursorCrossFeed
	}
	if table != "" && state.drop != drop {
		return state, store.ErrCursorExpired
	}
	if s.changeRetention > 0 && (now.After(state.issued.Add(s.changeRetention)) || now.After(state.start.Add(2*s.changeRetention))) {
		return state, store.ErrCursorExpired
	}
	_, err = tx.Exec(ctx, "UPDATE "+s.relation("cursors")+" SET issued_at=GREATEST(issued_at,$1) WHERE namespace=$2 AND token=$3", now, n.name, string(token))
	return state, err
}

func (s *Store) pruneChanges(ctx context.Context, tx pgx.Tx, n namespace, now time.Time) error {
	if s.changeRetention <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, "DELETE FROM "+s.relation("cursors")+" WHERE namespace=$1 AND (issued_at<$2 OR chain_start<$3)", n.name, now.Add(-s.changeRetention), now.Add(-2*s.changeRetention)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "DELETE FROM "+s.relation("changes")+" WHERE namespace=$1 AND created_at<$2 AND position<=COALESCE((SELECT min(chain_origin) FROM "+s.relation("cursors")+" WHERE namespace=$1),9223372036854775807)", n.name, now.Add(-2*s.changeRetention))
	return err
}

func (s *Store) ChangesSince(ctx context.Context, ns, table string, from store.Cursor, expected [16]byte, scope *store.RowScope, inc store.Incarnation, page store.Page) ([]store.ChangeRecord, store.Cursor, error) {
	if scope != nil {
		return nil, "", derr.New(derr.Forbidden, "PostgreSQL row scopes are not implemented yet")
	}
	records := []store.ChangeRecord{}
	var next store.Cursor
	err := s.write(ctx, ns, expected, func(tx pgx.Tx, n namespace) error {
		now := s.now()
		drop := int64(0)
		if table != "" {
			state, err := s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			if err := checkIncarnation(ns, state.incarnation, inc); err != nil {
				return err
			}
			drop = state.incarnation.DropGen
		} else if inc.NsGen != [16]byte{} && inc.NsGen != n.generation {
			return fmt.Errorf("%w: namespace was replaced", store.ErrNotFound)
		}
		var state cursorState
		if from != "" && from != store.CursorBegin {
			var err error
			state, err = s.resolveCursor(ctx, tx, n, from, table, drop, now)
			if err != nil {
				return err
			}
		} else {
			if err := tx.QueryRow(ctx, "SELECT next_change FROM "+s.relation("namespaces")+" WHERE name=$1", ns).Scan(&state.position); err != nil {
				return err
			}
			if from == store.CursorBegin {
				var first *int64
				stmt := "SELECT min(position) FROM " + s.relation("changes") + " WHERE namespace=$1"
				args := []any{ns}
				if s.changeRetention > 0 {
					stmt += " AND created_at >= $2"
					args = append(args, now.Add(-s.changeRetention))
				}
				if err := tx.QueryRow(ctx, stmt, args...).Scan(&first); err != nil {
					return err
				}
				if first != nil {
					state.position = *first - 1
				}
			}
			state.origin = state.position
			state.start = now
			state.table = table
			state.drop = drop
		}
		limit := page.Limit
		if limit <= 0 {
			limit = store.DefaultChangesPageLimit
		}
		if limit > store.MaxChangesPageLimit {
			limit = store.MaxChangesPageLimit
		}
		stmt := "SELECT position,table_name,drop_generation,row_id,kind FROM " + s.relation("changes") + " WHERE namespace=$1 AND position>$2"
		args := []any{ns, state.position}
		if table != "" {
			stmt += " AND table_name=$3 AND drop_generation=$4"
			args = append(args, table, drop)
		}
		stmt += fmt.Sprintf(" ORDER BY position LIMIT $%d", len(args)+1)
		args = append(args, limit)
		rows, err := tx.Query(ctx, stmt, args...)
		if err != nil {
			return err
		}
		positions := []int64{}
		for rows.Next() {
			var rec store.ChangeRecord
			var position int64
			if err := rows.Scan(&position, &rec.Table, &rec.Lifetime.DropGen, &rec.RowID, &rec.Kind); err != nil {
				rows.Close()
				return err
			}
			rec.Lifetime.NsGen = n.generation
			rec.Lifetime.Table = rec.Table
			records = append(records, rec)
			positions = append(positions, position)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i, position := range positions {
			state.position = position
			token, err := s.mintCursor(ctx, tx, n, state, now)
			if err != nil {
				return err
			}
			records[i].Cursor = token
		}
		if len(records) == 0 && from != "" && from != store.CursorBegin {
			next = from
		} else {
			var err error
			next, err = s.mintCursor(ctx, tx, n, state, now)
			if err != nil {
				return err
			}
		}
		return s.pruneChanges(ctx, tx, n, now)
	})
	if err != nil {
		return nil, "", err
	}
	return records, next, nil
}
