package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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

func (s *Store) mintCursors(ctx context.Context, tx pgx.Tx, n namespace, state cursorState, positions []int64, now time.Time) ([]store.Cursor, error) {
	if len(positions) == 0 {
		return nil, nil
	}
	tokens := make([]store.Cursor, len(positions))
	rows := make([]string, len(positions))
	args := make([]any, 0, len(positions)*8)
	for i, position := range positions {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		token := hex.EncodeToString(raw[:])
		tokens[i] = store.Cursor(token)
		base := i * 8
		rows[i] = fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)", base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8)
		args = append(args, n.name, token, position, state.origin, state.start, now, state.table, state.drop)
	}
	_, err := tx.Exec(ctx, "INSERT INTO "+s.relation("cursors")+" (namespace,token,position,chain_origin,chain_start,issued_at,table_name,drop_generation) VALUES "+strings.Join(rows, ","), args...)
	if err != nil {
		return nil, err
	}
	return tokens, nil
}

func (s *Store) namespaceGone(ctx context.Context, tx pgx.Tx, ns string, generation [16]byte) bool {
	var raw []byte
	err := tx.QueryRow(ctx, "SELECT generation FROM "+s.relation("namespaces")+" WHERE name=$1", ns).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	if err != nil {
		return false
	}
	return len(raw) != 16 || [16]byte(raw) != generation
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
	state.drop = drop
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

type feedMode int

const (
	feedRequest feedMode = iota
	feedReplay
	feedLive
)

func (s *Store) ChangesSince(ctx context.Context, ns, table string, from store.Cursor, expected [16]byte, scope *store.RowScope, inc store.Incarnation, page store.Page) ([]store.ChangeRecord, store.Cursor, error) {
	if scope != nil && table == "" {
		return nil, "", store.ErrScopedNamespaceFeed
	}
	if scope != nil {
		stale, serr := s.unlabeledBacklog(ctx, ns, table, from)
		if serr != nil {
			return nil, "", serr
		}
		if stale {
			return nil, "", store.ErrScopedFeedPredatesLabels
		}
	}
	return s.changesSinceMode(ctx, ns, table, from, expected, scope, inc, page, nil, feedRequest)
}

func (s *Store) changesSinceMode(ctx context.Context, ns, table string, from store.Cursor, expected [16]byte, scope *store.RowScope, inc store.Incarnation, page store.Page, boundary *int64, mode feedMode) ([]store.ChangeRecord, store.Cursor, error) {
	records := []store.ChangeRecord{}
	var next store.Cursor
	enter := s.write
	if mode == feedLive {
		enter = s.writeUnlocked
	}
	err := enter(ctx, ns, expected, func(tx pgx.Tx, n namespace) error {
		now := s.now()
		drop := int64(0)
		if table != "" {
			state, err := s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			if mode == feedRequest {
				if err := s.guardScope(ctx, tx, n, table, state, inc); err != nil {
					return err
				}
			} else if err := checkIncarnation(ns, state.incarnation, inc); err != nil {
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
				if mode == feedLive && errors.Is(err, store.ErrCursorExpired) && s.namespaceGone(ctx, tx, ns, n.generation) {
					return fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, ns)
				}
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
		stmt := "SELECT position,table_name,drop_generation,row_id,kind,owner FROM " + s.relation("changes") + " WHERE namespace=$1 AND position>$2"
		args := []any{ns, state.position}
		if table != "" {
			stmt += " AND table_name=$3 AND drop_generation=$4"
			args = append(args, table, drop)
		}
		if scope != nil {
			if scope.Empty {
				stmt += " AND false"
			} else {
				stmt += fmt.Sprintf(" AND owner=$%d", len(args)+1)
				args = append(args, scope.Owner)
			}
		}
		if boundary != nil {
			stmt += fmt.Sprintf(" AND position<=$%d", len(args)+1)
			args = append(args, *boundary)
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
			var owner *string
			if err := rows.Scan(&position, &rec.Table, &rec.Lifetime.DropGen, &rec.RowID, &rec.Kind, &owner); err != nil {
				rows.Close()
				return err
			}
			if owner != nil {
				rec.Owner = *owner
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
		tokens, err := s.mintCursors(ctx, tx, n, state, positions, now)
		if err != nil {
			return err
		}
		for i := range tokens {
			records[i].Cursor = tokens[i]
		}
		if len(positions) > 0 {
			state.position = positions[len(positions)-1]
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

func (s *Store) changeHead(ctx context.Context, ns string, expected [16]byte) (int64, error) {
	var head int64
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		return tx.QueryRow(ctx, "SELECT next_change FROM "+s.relation("namespaces")+" WHERE name=$1", ns).Scan(&head)
	})
	return head, err
}

func (s *Store) unlabeledBacklog(ctx context.Context, ns, table string, from store.Cursor) (bool, error) {
	stale := false
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		var head int64
		if herr := tx.QueryRow(ctx, "SELECT next_change FROM "+s.relation("namespaces")+" WHERE name=$1", ns).Scan(&head); herr != nil {
			return herr
		}
		drop := int64(0)
		if table != "" {
			state, terr := s.loadTable(ctx, tx, n, table)
			if terr != nil {
				return terr
			}
			drop = state.incarnation.DropGen
		}
		position := int64(0)
		switch {
		case from == "":
			position = head
		case from == store.CursorBegin:
			now := s.now()
			var first *int64
			stmt := "SELECT min(position) FROM " + s.relation("changes") + " WHERE namespace=$1"
			args := []any{n.name}
			if s.changeRetention > 0 {
				stmt += " AND created_at >= $2"
				args = append(args, now.Add(-s.changeRetention))
			}
			if berr := tx.QueryRow(ctx, stmt, args...).Scan(&first); berr != nil {
				return berr
			}
			if first == nil {
				position = head
			} else {
				position = *first - 1
			}
		default:
			state, cerr := s.resolveCursor(ctx, tx, n, from, table, drop, s.now())
			if cerr != nil {
				return cerr
			}
			position = state.position
		}
		if position >= head {
			return nil
		}
		stmt := "SELECT 1 FROM " + s.relation("changes") + " WHERE namespace=$1 AND position>$2 AND position<=$3 AND owner IS NULL"
		args := []any{n.name, position, head}
		if table != "" {
			stmt += " AND table_name=$4 AND drop_generation=$5"
			args = append(args, table, drop)
		}
		var one int
		qerr := tx.QueryRow(ctx, stmt+" LIMIT 1", args...).Scan(&one)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return nil
		}
		if qerr != nil {
			return qerr
		}
		stale = true
		return nil
	})
	return stale, err
}

func (s *Store) anchorListen(ctx context.Context, ns, table string, from store.Cursor, expected [16]byte, inc store.Incarnation) (store.Cursor, store.Cursor, int64, error) {
	var replay, live store.Cursor
	var head int64
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
		}
		if err := tx.QueryRow(ctx, "SELECT next_change FROM "+s.relation("namespaces")+" WHERE name=$1", ns).Scan(&head); err != nil {
			return err
		}
		token, err := s.mintCursor(ctx, tx, n, cursorState{position: head, origin: head, start: now, table: table, drop: drop}, now)
		if err != nil {
			return err
		}
		live = token
		if from != "" && from != store.CursorBegin {
			replay = from
			return nil
		}
		state := cursorState{position: head, origin: head, start: now, table: table, drop: drop}
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
				state.origin = state.position
			}
		}
		replay, err = s.mintCursor(ctx, tx, n, state, now)
		return err
	})
	return replay, live, head, err
}

func (s *Store) anchorCursor(ctx context.Context, ns, table string, from store.Cursor, expected [16]byte, inc store.Incarnation) (store.Cursor, error) {
	if from != "" && from != store.CursorBegin {
		return from, nil
	}
	var token store.Cursor
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
		}
		var state cursorState
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
		var err error
		token, err = s.mintCursor(ctx, tx, n, state, now)
		return err
	})
	return token, err
}

func (s *Store) reanchorCursor(ctx context.Context, ns, table string, from store.Cursor, expected [16]byte, inc store.Incarnation) (store.Cursor, error) {
	if from == "" || from == store.CursorBegin {
		return from, nil
	}
	token := from
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
		}
		state, err := s.resolveCursor(ctx, tx, n, from, table, drop, now)
		if err != nil {
			return err
		}
		state.origin = state.position
		state.start = now
		token, err = s.mintCursor(ctx, tx, n, state, now)
		if err != nil {
			return err
		}
		return s.pruneChanges(ctx, tx, n, now)
	})
	return token, err
}
