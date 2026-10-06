package lakehouse

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

type cursorState struct {
	position int64
	origin   int64
	start    int64
	table    string
	drop     int64
}

func mintCursor(ctx context.Context, tx *sql.Tx, state cursorState, now time.Time) (store.Cursor, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	_, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_cursors(token, position, chain_origin, chain_start, issued_at, table_name, drop_generation) VALUES(?,?,?,?,?,?,?)`, token, state.position, state.origin, state.start, now.UnixNano(), state.table, state.drop)
	return store.Cursor(token), err
}

func (s *Store) resolveCursor(ctx context.Context, tx *sql.Tx, token store.Cursor, table string, drop int64, now time.Time) (cursorState, error) {
	var state cursorState
	var issued int64
	err := tx.QueryRowContext(ctx, `SELECT position, chain_origin, chain_start, issued_at, table_name, drop_generation FROM _dolmen_lakehouse_cursors WHERE token = ?`, string(token)).Scan(&state.position, &state.origin, &state.start, &issued, &state.table, &state.drop)
	if errors.Is(err, sql.ErrNoRows) {
		return state, store.ErrCursorExpired
	}
	if err != nil {
		return state, err
	}
	if state.table != table {
		return state, store.ErrCursorCrossFeed
	}
	state.drop = drop
	if s.retention > 0 && (now.UnixNano() > issued+int64(s.retention) || now.UnixNano() > state.start+2*int64(s.retention)) {
		return state, store.ErrCursorExpired
	}
	_, err = tx.ExecContext(ctx, `UPDATE _dolmen_lakehouse_cursors SET issued_at = max(issued_at, ?) WHERE token = ?`, now.UnixNano(), string(token))
	return state, err
}

func (s *Store) pruneChanges(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if s.retention <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_cursors WHERE issued_at < ? OR chain_start < ?`, now.Add(-s.retention).UnixNano(), now.Add(-2*s.retention).UnixNano()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_changes WHERE at < ? AND seq <= COALESCE((SELECT min(chain_origin) FROM _dolmen_lakehouse_cursors), 9223372036854775807)`, now.Add(-2*s.retention).UTC().Format("2006-01-02T15:04:05.000Z"))
	return err
}

func changeHead(ctx context.Context, tx *sql.Tx) (int64, error) {
	var head int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = '_dolmen_lakehouse_changes'), 0)`).Scan(&head)
	return head, err
}

func (s *Store) ChangesSince(ctx context.Context, ns, table string, from store.Cursor, expected [16]byte, scope *store.RowScope, inc store.Incarnation, page store.Page) ([]store.ChangeRecord, store.Cursor, error) {
	if scope != nil && table == "" {
		return nil, "", store.ErrScopedNamespaceFeed
	}
	records := []store.ChangeRecord{}
	var next store.Cursor
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		if expected != [16]byte{} && expected != n.generation {
			return fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, ns)
		}
		drop := int64(0)
		if table != "" {
			state, err := loadTable(ctx, n, ns, table)
			if err != nil {
				return err
			}
			if err := checkScopeExpected(state, inc); err != nil {
				return err
			}
			if scope != nil && !scope.Empty && !state.schema.HasOwner {
				return invalidf("table %s carries no owner column, so a row scope cannot be applied to it", table)
			}
			drop = state.incarnation.DropGen
		} else if inc.NsGen != [16]byte{} && inc.NsGen != n.generation {
			return fmt.Errorf("%w: namespace was replaced", store.ErrNotFound)
		}
		now := time.Now()
		tx, err := n.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var state cursorState
		if from != "" && from != store.CursorBegin {
			if state, err = s.resolveCursor(ctx, tx, from, table, drop, now); err != nil {
				return err
			}
		} else {
			if state.position, err = changeHead(ctx, tx); err != nil {
				return err
			}
			if from == store.CursorBegin {
				var first sql.NullInt64
				stmt := `SELECT min(seq) FROM _dolmen_lakehouse_changes`
				var args []any
				if s.retention > 0 {
					stmt += ` WHERE at >= ?`
					args = append(args, now.Add(-s.retention).UTC().Format("2006-01-02T15:04:05.000Z"))
				}
				if err := tx.QueryRowContext(ctx, stmt, args...).Scan(&first); err != nil {
					return err
				}
				if first.Valid {
					state.position = first.Int64 - 1
				}
			}
			state.origin = state.position
			state.start = now.UnixNano()
			state.table = table
			state.drop = drop
		}
		if scope != nil {
			head, err := changeHead(ctx, tx)
			if err != nil {
				return err
			}
			if state.position < head {
				var one int
				switch qerr := tx.QueryRowContext(ctx, `SELECT 1 FROM _dolmen_lakehouse_changes WHERE seq > ? AND seq <= ? AND owner IS NULL AND table_name = ? AND generation = ? LIMIT 1`, state.position, head, table, drop).Scan(&one); {
				case qerr == nil:
					return store.ErrScopedFeedPredatesLabels
				case !errors.Is(qerr, sql.ErrNoRows):
					return qerr
				}
			}
		}
		limit := page.Limit
		if limit <= 0 {
			limit = store.DefaultChangesPageLimit
		}
		if limit > store.MaxChangesPageLimit {
			limit = store.MaxChangesPageLimit
		}
		stmt := `SELECT seq, table_name, generation, row_id, kind, owner, commit_id FROM _dolmen_lakehouse_changes WHERE seq > ?`
		args := []any{state.position}
		if table != "" {
			stmt += ` AND table_name = ? AND generation = ?`
			args = append(args, table, drop)
		}
		if scope != nil {
			if scope.Empty {
				stmt += ` AND 0`
			} else {
				stmt += ` AND owner = ?`
				args = append(args, scope.Owner)
			}
		}
		stmt += ` ORDER BY seq LIMIT ?`
		args = append(args, limit)
		rows, err := tx.QueryContext(ctx, stmt, args...)
		if err != nil {
			return err
		}
		var positions []int64
		for rows.Next() {
			var rec store.ChangeRecord
			var position int64
			var owner sql.NullString
			var kind string
			if err := rows.Scan(&position, &rec.Table, &rec.Lifetime.DropGen, &rec.RowID, &kind, &owner, &rec.Commit); err != nil {
				rows.Close()
				return err
			}
			rec.Kind = store.ChangeKind(kind)
			rec.Owner = owner.String
			rec.Lifetime.NsGen = n.generation
			rec.Lifetime.Table = rec.Table
			records = append(records, rec)
			positions = append(positions, position)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i, p := range positions {
			st := state
			st.position = p
			if records[i].Cursor, err = mintCursor(ctx, tx, st, now); err != nil {
				return err
			}
		}
		if len(positions) > 0 {
			state.position = positions[len(positions)-1]
		}
		if len(records) == 0 && from != "" && from != store.CursorBegin {
			next = from
		} else if next, err = mintCursor(ctx, tx, state, now); err != nil {
			return err
		}
		if err := s.pruneChanges(ctx, tx, now); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, "", err
	}
	return records, next, nil
}
