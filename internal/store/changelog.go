package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

func mintChanges(ctx context.Context, tx *sql.Tx, table string, kind ChangeKind, ids []int64, owners []string) (ChangeRange, error) {
	if len(ids) == 0 {
		return ChangeRange{}, nil
	}
	if owners != nil && len(owners) != len(ids) {
		return ChangeRange{}, fmt.Errorf("mint change records for %s: %d owner labels for %d ids", table, len(owners), len(ids))
	}
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		return ChangeRange{}, err
	}
	nsGen, err := readNSGen(ctx, tx)
	if err != nil {
		return ChangeRange{}, err
	}
	var first, last int64
	for i, id := range ids {

		var owner any
		if owners != nil && owners[i] != "" {
			owner = owners[i]
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO _dolmen_changes(table_name, row_id, kind, owner, nsgen, drop_gen) VALUES(?,?,?,?,?,?)`,
			table, id, string(kind), owner, nsGen[:], gen)
		if err != nil {
			return ChangeRange{}, fmt.Errorf("mint change records for %s: %w", table, err)
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return ChangeRange{}, err
		}
		if i == 0 {
			first = seq
		}
		last = seq
	}
	return ChangeRange{First: first, Last: last, Count: int64(len(ids))}, nil
}

func changeOwnerColumn(sc *schema.TableSchema) string {
	if sc != nil && sc.HasOwner {
		return q(schema.OwnerColumn)
	}
	return `NULL`
}

func sameOwner(owner string, n int) []string {
	if owner == "" {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		out[i] = owner
	}
	return out
}

func mintChangesFromTemp(ctx context.Context, tx *sql.Tx, table string, kind ChangeKind, temp string) (ChangeRange, error) {
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		return ChangeRange{}, err
	}
	nsGen, err := readNSGen(ctx, tx)
	if err != nil {
		return ChangeRange{}, err
	}
	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO _dolmen_changes(table_name, row_id, kind, owner, nsgen, drop_gen) SELECT ?, id, ?, owner, ?, ? FROM %s ORDER BY id`, temp),
		table, string(kind), nsGen[:], gen)
	if err != nil {
		return ChangeRange{}, fmt.Errorf("mint change records for %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ChangeRange{}, err
	}
	if n == 0 {
		return ChangeRange{}, nil
	}
	last, err := res.LastInsertId()
	if err != nil {
		return ChangeRange{}, err
	}
	return ChangeRange{First: last - n + 1, Last: last, Count: n}, nil
}

var ErrScopedNamespaceFeed = fmt.Errorf("%w: this request is scoped to your own rows, and the namespace-wide feed reports every table; name a table, or ask for the read verb on the namespace", ErrInvalid)

var (
	ErrCursorExpired = errors.New("cursor token is unknown or past retention")

	ErrCursorCrossFeed = errors.New("cursor token belongs to a different feed")
)

type cursorDB interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const cursorTokenBytes = 16

type cursorChain struct {
	ID     string
	Origin int64
	Start  int64
}

type cursorRow struct {
	Token       string
	Position    int64
	IssuedAt    int64
	ChainID     string
	ChainOrigin int64
	ChainStart  int64
	FeedTable   string
}

func changeHead(ctx context.Context, db rowQuerier) (int64, error) {
	var head int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM _dolmen_changes`).Scan(&head); err != nil {
		return 0, err
	}
	return head, nil
}

func changeBegin(ctx context.Context, db rowQuerier, now time.Time, retention time.Duration) (int64, error) {

	if retention <= 0 {
		var oldest sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT MIN(seq) FROM _dolmen_changes`).Scan(&oldest); err != nil {
			return 0, err
		}
		if !oldest.Valid {
			return 0, nil
		}
		return oldest.Int64 - 1, nil
	}

	var first sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MIN(seq) FROM _dolmen_changes WHERE at >= ?`,
		isoChangeStamp(now.Add(-retention))).Scan(&first); err != nil {
		return 0, err
	}
	if !first.Valid {

		return changeHead(ctx, db)
	}
	return first.Int64 - 1, nil
}

func mintCursorToken(ctx context.Context, db cursorDB, now time.Time, position int64, feedTable string, chain *cursorChain) (Cursor, error) {
	var raw [cursorTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint cursor token: %w", err)
	}
	if chain == nil {
		chain = newCursorChain(now, position)
	}
	tok := Cursor(hex.EncodeToString(raw[:]))
	if _, err := db.ExecContext(ctx,
		`INSERT INTO _dolmen_cursor_tokens(token, position, issued_at, chain_id, chain_origin, chain_start, feed_table) VALUES(?,?,?,?,?,?,?)`,
		string(tok), position, now.UnixMilli(), chain.ID, chain.Origin, chain.Start, feedTable); err != nil {
		return "", err
	}
	return tok, nil
}

func newCursorChain(now time.Time, position int64) *cursorChain {
	var cid [cursorTokenBytes]byte
	if _, err := rand.Read(cid[:]); err != nil {

		return &cursorChain{Origin: position, Start: now.UnixMilli()}
	}
	return &cursorChain{ID: hex.EncodeToString(cid[:]), Origin: position, Start: now.UnixMilli()}
}

func resolveCursorToken(ctx context.Context, db cursorDB, now time.Time, retention time.Duration, tok Cursor, feedTable string) (cursorRow, error) {
	var row cursorRow
	err := db.QueryRowContext(ctx,
		`SELECT token, position, issued_at, chain_id, chain_origin, chain_start, feed_table FROM _dolmen_cursor_tokens WHERE token = ?`,
		string(tok)).Scan(&row.Token, &row.Position, &row.IssuedAt, &row.ChainID, &row.ChainOrigin, &row.ChainStart, &row.FeedTable)
	if errors.Is(err, sql.ErrNoRows) {
		return cursorRow{}, ErrCursorExpired
	}
	if err != nil {
		return cursorRow{}, err
	}
	if row.FeedTable != feedTable {
		return cursorRow{}, fmt.Errorf("%w: minted for feed %q, resolved for feed %q", ErrCursorCrossFeed, row.FeedTable, feedTable)
	}
	if retention > 0 {
		nowMs := now.UnixMilli()
		rms := int64(retention / time.Millisecond)
		if nowMs > row.IssuedAt+rms {
			return cursorRow{}, ErrCursorExpired
		}
		if nowMs > row.ChainStart+2*rms {
			return cursorRow{}, ErrCursorExpired
		}
	}
	res, err := db.ExecContext(ctx,
		`UPDATE _dolmen_cursor_tokens SET issued_at = MAX(issued_at, ?) WHERE token = ?`,
		now.UnixMilli(), row.Token)
	if err != nil {
		return cursorRow{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return cursorRow{}, err
	}
	if n == 0 {
		return cursorRow{}, ErrCursorExpired
	}
	return row, nil
}

func pruneChanges(ctx context.Context, db cursorDB, now time.Time, retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	rms := int64(retention / time.Millisecond)
	nowMs := now.UnixMilli()
	if _, err := db.ExecContext(ctx,
		`DELETE FROM _dolmen_cursor_tokens WHERE issued_at < ?`, nowMs-rms); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM _dolmen_cursor_tokens WHERE chain_start < ?`, nowMs-2*rms); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		`DELETE FROM _dolmen_changes
		 WHERE at < ?
		   AND seq <= COALESCE((SELECT MIN(chain_origin) FROM _dolmen_cursor_tokens),
		                        9223372036854775807)`,
		isoChangeStamp(now.Add(-2*retention)))
	return err
}

func isoChangeStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

const (
	DefaultChangesPageLimit = 100
	MaxChangesPageLimit     = 1000
)

func changesPageLimit(n int) int {
	if n <= 0 {
		return DefaultChangesPageLimit
	}
	if n > MaxChangesPageLimit {
		return MaxChangesPageLimit
	}
	return n
}

type loggedChange struct {
	rec ChangeRecord
	seq int64
}

type changeFeed struct {
	table   string
	nsgen   [16]byte
	dropGen int64
}

func changeFeedOf(ctx context.Context, tx *sql.Tx, nsName, table string) (*changeFeed, error) {
	if _, err := loadSchema(ctx, tx, nsName, table); err != nil {
		return nil, err
	}
	dropGen, err := tableGen(ctx, tx, table)
	if err != nil {
		return nil, err
	}
	nsgen, err := readNSGen(ctx, tx)
	if err != nil {
		return nil, err
	}
	return &changeFeed{table: table, nsgen: nsgen, dropGen: dropGen}, nil
}

var ErrScopedFeedPredatesLabels = fmt.Errorf("%w: this feed still retains changes recorded before rows carried an owner, and a caller restricted to their own rows cannot be shown them or told they were skipped; subscribe without a cursor to start at the current head, or ask for the read verb on the table, which lifts the scope", ErrInvalid)

func unlabeledChangeInRange(ctx context.Context, tx *sql.Tx, from, to int64, feed *changeFeed) (bool, error) {
	q := `SELECT 1 FROM _dolmen_changes WHERE seq > ? AND seq <= ? AND owner IS NULL`
	args := []any{from, to}
	if feed != nil {
		q += ` AND table_name = ? AND drop_gen = ? AND nsgen = ?`
		args = append(args, feed.table, feed.dropGen, feed.nsgen[:])
	}
	var one int
	err := tx.QueryRowContext(ctx, q+` LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func changeScopeSQL(scope *RowScope) (string, []any) {
	if scope == nil {
		return "", nil
	}
	if scope.Empty {
		return ` AND 0`, nil
	}
	return ` AND owner = ?`, []any{scope.Owner}
}

func changePageSQL(from int64, to *int64, limit int, feed *changeFeed, scope *RowScope) (string, []any) {
	q := `SELECT seq, table_name, row_id, kind, owner, nsgen, drop_gen FROM _dolmen_changes WHERE seq > ?`
	args := []any{from}
	if to != nil {
		q += ` AND seq <= ?`
		args = append(args, *to)
	}
	if feed != nil {
		q += ` AND table_name = ? AND drop_gen = ? AND nsgen = ?`
		args = append(args, feed.table, feed.dropGen, feed.nsgen[:])
	}
	sq, sargs := changeScopeSQL(scope)
	return q + sq + ` ORDER BY seq LIMIT ?`, append(append(args, sargs...), limit)
}

func scanChangePage(rows *sql.Rows) ([]loggedChange, error) {
	var scanned []loggedChange
	for rows.Next() {
		var lc loggedChange
		var owner sql.NullString
		var gen []byte
		if err := rows.Scan(&lc.seq, &lc.rec.Table, &lc.rec.RowID, &lc.rec.Kind, &owner, &gen, &lc.rec.Lifetime.DropGen); err != nil {
			return nil, err
		}
		if len(gen) != 16 {
			return nil, fmt.Errorf("corrupt change record %d: nsgen is %d bytes, want 16", lc.seq, len(gen))
		}
		copy(lc.rec.Lifetime.NsGen[:], gen)
		lc.rec.Lifetime.Table = lc.rec.Table
		lc.rec.Owner = owner.String
		scanned = append(scanned, lc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return scanned, nil
}

func mintChangeCursors(ctx context.Context, tx *sql.Tx, now time.Time, scanned []loggedChange, resume int64, from Cursor, feedTable string, chain *cursorChain) ([]ChangeRecord, Cursor, error) {
	records := make([]ChangeRecord, len(scanned))
	for i := range scanned {
		tok, err := mintCursorToken(ctx, tx, now, scanned[i].seq, feedTable, chain)
		if err != nil {
			return nil, "", err
		}
		scanned[i].rec.Cursor = tok
		records[i] = scanned[i].rec
	}
	if len(scanned) == 0 && from != "" && from != CursorBegin {
		return records, from, nil
	}
	nextPos := resume
	if len(scanned) > 0 {
		nextPos = scanned[len(scanned)-1].seq
	}
	next, err := mintCursorToken(ctx, tx, now, nextPos, feedTable, chain)
	if err != nil {
		return nil, "", err
	}
	return records, next, nil
}

func (s *Store) ChangesSince(ctx context.Context, nsName, table string, from Cursor, nsGen [16]byte, scope *RowScope, scopeIncarnation Incarnation, page Page) ([]ChangeRecord, Cursor, error) {
	if scope != nil && table == "" {
		return nil, "", ErrScopedNamespaceFeed
	}
	if table != "" {
		if err := s.guardIncarnation(ctx, nsName, table, scopeIncarnation); err != nil {
			return nil, "", err
		}
	}
	n, err := s.nsCtx(ctx, nsName)
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()

	var feed *changeFeed
	if table != "" {
		var ferr error
		if feed, ferr = changeFeedOf(ctx, tx, nsName, table); ferr != nil {
			return nil, "", ferr
		}
		sc, serr := loadSchema(ctx, tx, nsName, table)
		if serr != nil {
			return nil, "", serr
		}
		if serr := scopeUsable(scope, sc); serr != nil {
			return nil, "", serr
		}
	}

	var position int64
	var chain *cursorChain
	switch {
	case from == "":
		if position, err = changeHead(ctx, tx); err != nil {
			return nil, "", err
		}
		chain = newCursorChain(now, position)
	case from == CursorBegin:
		if position, err = changeBegin(ctx, tx, now, s.changeRetention); err != nil {
			return nil, "", err
		}
		chain = newCursorChain(now, position)
	default:
		var row cursorRow
		if row, err = resolveCursorToken(ctx, tx, now, s.changeRetention, from, table); err != nil {
			return nil, "", err
		}
		position = row.Position
		chain = &cursorChain{ID: row.ChainID, Origin: row.ChainOrigin, Start: row.ChainStart}
	}

	if scope != nil {
		head, herr := changeHead(ctx, tx)
		if herr != nil {
			return nil, "", herr
		}
		if position < head {
			stale, serr := unlabeledChangeInRange(ctx, tx, position, head, feed)
			if serr != nil {
				return nil, "", serr
			}
			if stale {
				return nil, "", ErrScopedFeedPredatesLabels
			}
		}
	}

	limit := changesPageLimit(page.Limit)
	query, args := changePageSQL(position, nil, limit, feed, scope)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	scanned, err := scanChangePage(rows)
	rows.Close()
	if err != nil {
		return nil, "", err
	}

	records, next, err := mintChangeCursors(ctx, tx, now, scanned, position, from, table, chain)
	if err != nil {
		return nil, "", err
	}

	if err := pruneChanges(ctx, tx, now, s.changeRetention); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return records, next, nil
}

func changeCountSQL(from, to int64, feed *changeFeed) (string, []any) {
	q := `SELECT COUNT(*) FROM _dolmen_changes WHERE seq > ? AND seq <= ?`
	args := []any{from, to}
	if feed != nil {
		q += ` AND table_name = ? AND drop_gen = ? AND nsgen = ?`
		args = append(args, feed.table, feed.dropGen, feed.nsgen[:])
	}
	return q, args
}
