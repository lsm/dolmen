package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// mintChanges appends one change record per affected id to the namespace's
// durable change log (§9.3), INSIDE the caller's write transaction: the
// records and the data write they describe commit or roll back together, so
// there is no crash window in which an acknowledged write is missing from the
// log, and a rolled-back write leaves zero rows behind. The AUTOINCREMENT seq
// assignment makes each call's positions contiguous — writers serialize on the
// database's write lock, so nothing can interleave between this transaction's
// inserts — and the seq IS the per-namespace cursor (§9.3: monotonic, gap-free
// by construction).
//
// Each row carries the lifetime labels a replay needs (§3.4): the table's
// CURRENT drop generation, read inside the same transaction so a record can
// never claim a generation whose lifetime it was not minted in, and the
// namespace's creation id. owners carries each row's OWN owner label — the
// row's, never a single caller value (§9.3: the label is the row's, so a
// table-wide writer must not relabel other users' rows); nil, or a ""
// entry, mints NULL — the label until owner stamping lands (slice 9c).
//
// The returned ChangeRange identifies exactly what this call minted; the
// caller attaches it to the write result (§6.2). The records themselves are
// never materialized on the write path — a bulk write has no match-count cap,
// and a routine mutation must not construct an in-memory copy of the durable
// log it just wrote (§6.2).
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
		// nil owner writes SQL NULL — the ChangeRecord convention maps the
		// empty string back to "no owner label" on read (§6.2).
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

// mintChangesFromTemp is mintChanges for the writes whose affected rows the
// transaction already materialized into a temp id table — _dolmen_update_ids
// for updates and upserts, _dolmen_delete_ids for deletes. The mint is a
// single INSERT…SELECT out of that table: the ids never round-trip through
// the server, so a bulk update or confirmed delete — which has no match-count
// cap — cannot consume memory proportional to the table when the request
// itself was O(1) (§6.2). Only the scalar ChangeRange crosses back, and it is
// reconstructed from the statement's row count and last-insert id without
// reading a single row back: one statement assigns the AUTOINCREMENT seqs
// contiguously in insertion order, so the two scalars pin the whole range.
// Rows are minted in id order, like every other path; an empty temp table
// mints nothing and returns the zero range.
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
		fmt.Sprintf(`INSERT INTO _dolmen_changes(table_name, row_id, kind, owner, nsgen, drop_gen) SELECT ?, id, ?, NULL, ?, ? FROM %s ORDER BY id`, temp),
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

// ---------------------------------------------------------------------------
// Cursor tokens and retention (§9.3) — slice 5b.
//
// These helpers are the coordinated-storage mapping described in the
// _dolmen_cursor_tokens DDL: mint, resolve, head, the begin boundary, and
// time-based pruning. Slice 5c's ChangesSince is their consumer; they land
// first, pinned by their own tests, so the replay op is assembled from
// proven parts.

var (
	// errCursorExpired is the engine-internal shape of §9.3's beyond-retention
	// error: the presented token is unknown, pruned, past its own deadline,
	// or past its chain's absolute cap — either way it names no position. The
	// op layer (slice 5c) maps it to the explicit teaching error naming the
	// catch-up path; whether it fires is a function of the client's own token
	// age alone, never of what the log contains.
	errCursorExpired = errors.New("cursor token is unknown or past retention")

	// errCursorCrossFeed: the token was minted on a different feed (another
	// table's, or the unfiltered namespace feed) than the one it was resolved
	// against. Cross-feed reuse is rejected, never honored as a position —
	// honoring it would silently skip the target feed's events behind the
	// foreign feed's cursor (§9.3).
	errCursorCrossFeed = errors.New("cursor token belongs to a different feed")
)

// cursorDB is the handle the cursor helpers run against: resolution refreshes
// deadlines and pruning deletes, so they need both query and exec — satisfied
// by the namespace's writable pool and by a transaction alike.
type cursorDB interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// cursorTokenBytes is the entropy of a minted cursor token: 16 crypto-random
// bytes, hex-encoded to a fixed 32-character string. Pure noise per issuance —
// never derived from the position — so consecutive visible records yield
// unrelated tokens and no foreign commit is observable through cursor
// arithmetic (§9.3); the fixed length cannot leak an encoding boundary.
const cursorTokenBytes = 16

// cursorChain names the page chain a mint belongs to. A nil *cursorChain
// starts a NEW chain: mintCursorToken draws its id and fixes Origin (the
// chain's resume position — the begin-boundary semantics) and Start (the
// absolute-cap anchor), both immutable for the chain's life. A non-nil one
// inherits an existing chain's identity unchanged.
type cursorChain struct {
	ID     string
	Origin int64
	Start  int64 // unix ms
}

// cursorRow is one _dolmen_cursor_tokens row as resolve reads it back.
type cursorRow struct {
	Token       string
	Position    int64
	IssuedAt    int64 // unix ms
	ChainID     string
	ChainOrigin int64
	ChainStart  int64 // unix ms
	FeedTable   string
}

// changeHead returns the namespace change log's current head position: the
// highest minted seq, 0 when the log is empty. It is the resume position of
// both the zero cursor and a head-minted token (§9.3: an omitted cursor
// starts at the CURRENT HEAD — no backlog — and the response carries the head
// cursor, so only subsequent commits are delivered).
func changeHead(ctx context.Context, db rowQuerier) (int64, error) {
	var head int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM _dolmen_changes`).Scan(&head); err != nil {
		return 0, err
	}
	return head, nil
}

// changeBegin resolves the "begin" sentinel's resume position (§9.3): the
// oldest retained record with a full page-chain of headroom. A chain begun at
// time T caps at T+2R and each of its tokens dies at its own issuance + R,
// while a record minted at M is held to M+2R — so every record a chain can
// still reach satisfies M ≥ T−R, and the boundary is the newest record
// strictly older than R before the call: everything after it is readable
// through the whole chain, everything before it is outside every replay
// guarantee and skipped, never replayed into a chain that pruning could then
// break mid-page.
//
// With retention disabled (R = 0) records never age out and the headroom
// argument is vacuous — the formulas must NOT apply, or M ≥ T skips retained
// history: the boundary is the oldest retained record itself (§9.3).
//
// When no record qualifies — an empty log, or everything older than the
// window — the boundary is the current head: exactly the bare-start
// semantics, nothing retained is readable and the chain delivers future
// commits only.
//
// Records mint in seq order under the serialized write lock, so `at` never
// decreases with seq and the non-qualifying records are a prefix; MAX over
// that prefix is the boundary position.
func changeBegin(ctx context.Context, db rowQuerier, now time.Time, retention time.Duration) (int64, error) {
	if retention <= 0 {
		// Oldest retained record: the chain resumes after it — position
		// seq−1, which a never-pruned log makes 0. An empty log shares the
		// bare-start position (head is also 0).
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
	var pos int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM _dolmen_changes WHERE at < ?`,
		isoChangeStamp(now.Add(-retention))).Scan(&pos); err != nil {
		return 0, err
	}
	return pos, nil
}

// mintCursorToken stores one _dolmen_cursor_tokens row and returns its opaque
// token. chain == nil starts a new chain at this position and time; chain !=
// nil refreshes the page-chain deadline (§9.3): the new row carries a fresh
// issuance while inheriting the chain's id, origin, and start unchanged, so a
// promptly-paged backlog never fails between pages because an early token was
// nearly R old, and neither the boundary semantics nor the cap anchor ever
// drift across a chain.
func mintCursorToken(ctx context.Context, db cursorDB, now time.Time, position int64, feedTable string, chain *cursorChain) (Cursor, error) {
	var raw [cursorTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint cursor token: %w", err)
	}
	if chain == nil {
		var cid [cursorTokenBytes]byte
		if _, err := rand.Read(cid[:]); err != nil {
			return "", fmt.Errorf("mint cursor chain id: %w", err)
		}
		chain = &cursorChain{ID: hex.EncodeToString(cid[:]), Origin: position, Start: now.UnixMilli()}
	}
	tok := Cursor(hex.EncodeToString(raw[:]))
	if _, err := db.ExecContext(ctx,
		`INSERT INTO _dolmen_cursor_tokens(token, position, issued_at, chain_id, chain_origin, chain_start, feed_table) VALUES(?,?,?,?,?,?,?)`,
		string(tok), position, now.UnixMilli(), chain.ID, chain.Origin, chain.Start, feedTable); err != nil {
		return "", err
	}
	return tok, nil
}

// resolveCursorToken maps an opaque token back to its position and chain,
// enforcing §9.3's validity rules BEFORE the position is honored:
//
//   - Feed binding: the stored feed_table must equal the caller's table
//     selector ('' = the unfiltered namespace feed). Anything else is
//     cross-feed reuse — rejected, never honored.
//
//   - The token's own deadline: now ≤ issued_at + R. Retention 0 disables
//     every expiry — cursors never expire, no matter their age.
//
//   - The chain's absolute cap: now ≤ chain_start + 2R. Refreshing extends a
//     chain's deadlines but never past the cap, so a client cannot keep an
//     old backlog alive forever and defeat -change-retention (§9.3).
//
// A successful resolve also refreshes the PRESENTED token's deadline — a
// legitimate retry must not die because the first response was lost — by
// updating issued_at in place; the cap cannot move with it, because
// chain_start is immutable per chain. That refresh is also the atomic
// existence re-check: on the shared rw pool the SELECT and the UPDATE are
// separate statements, and a concurrent pruneChanges may delete the token —
// and the records it can still reach — between them. An UPDATE that matched
// zero rows means exactly that: the position must not be honored, or replay
// would be shortened under a live cursor (§9.3).
func resolveCursorToken(ctx context.Context, db cursorDB, now time.Time, retention time.Duration, tok Cursor, feedTable string) (cursorRow, error) {
	var row cursorRow
	err := db.QueryRowContext(ctx,
		`SELECT token, position, issued_at, chain_id, chain_origin, chain_start, feed_table FROM _dolmen_cursor_tokens WHERE token = ?`,
		string(tok)).Scan(&row.Token, &row.Position, &row.IssuedAt, &row.ChainID, &row.ChainOrigin, &row.ChainStart, &row.FeedTable)
	if errors.Is(err, sql.ErrNoRows) {
		return cursorRow{}, errCursorExpired
	}
	if err != nil {
		return cursorRow{}, err
	}
	if row.FeedTable != feedTable {
		return cursorRow{}, fmt.Errorf("%w: minted for feed %q, resolved for feed %q", errCursorCrossFeed, row.FeedTable, feedTable)
	}
	if retention > 0 {
		nowMs := now.UnixMilli()
		rms := int64(retention / time.Millisecond)
		if nowMs > row.IssuedAt+rms {
			return cursorRow{}, errCursorExpired
		}
		if nowMs > row.ChainStart+2*rms {
			return cursorRow{}, errCursorExpired
		}
	}
	res, err := db.ExecContext(ctx,
		`UPDATE _dolmen_cursor_tokens SET issued_at = ? WHERE token = ?`,
		now.UnixMilli(), row.Token)
	if err != nil {
		return cursorRow{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return cursorRow{}, err
	}
	if n == 0 {
		return cursorRow{}, errCursorExpired
	}
	return row, nil
}

// pruneChanges applies §9.3's retention, TIME-based only: never a record
// count or byte cap, which would let foreign commits evict a scoped reader's
// cursor while that reader has received nothing visible and turn an
// otherwise-empty resume into an error that reveals hidden traffic. Retention
// 0 disables pruning entirely — records and tokens accumulate.
//
// Tokens are pruned once past their own deadline (issued_at + R) or their
// chain's absolute cap (chain_start + 2R), whichever comes first.
//
// Records are pruned when older than the 2R hold AND beyond the reach of
// every surviving chain: a chain retains everything past its origin through
// its current deadline, so refresh and retention move together and a
// refreshed next-page token never meets a pruned page. Reading the boundary
// from the durable chain rows — not age alone — is what keeps a mid-replay
// chain gap-free: at the cap its oldest records can be nearly 3R old while
// remaining perfectly valid; pure age-R pruning would break gap-free replay
// with a beyond-retention error or a shortened page on a perfectly valid
// cursor. The reach boundary is the chains' minimum origin, precomputed once
// by the scalar subquery — a correlated per-record EXISTS would rescan the
// token table for every candidate record, and both tables grow for the whole
// retention window. With no surviving chains the minimum is NULL and every
// age-eligible record goes.
func pruneChanges(ctx context.Context, db cursorDB, now time.Time, retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	rms := int64(retention / time.Millisecond)
	nowMs := now.UnixMilli()
	if _, err := db.ExecContext(ctx,
		`DELETE FROM _dolmen_cursor_tokens WHERE issued_at + ? < ? OR chain_start + 2 * ? < ?`,
		rms, nowMs, rms, nowMs); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		`DELETE FROM _dolmen_changes
		 WHERE at < ?
		   AND seq <= COALESCE((SELECT MIN(chain_origin) FROM _dolmen_cursor_tokens),
		                        9223372036854775807)`,
		isoChangeStamp(now.Add(-2 * retention)))
	return err
}

// isoChangeStamp renders t exactly as the log's `at` column stamps it —
// SQLite's '%Y-%m-%dT%H:%M:%fZ': UTC with a fixed 3-digit millisecond field —
// so boundary and pruning cutoffs compare against `at` as plain strings:
// identical shapes make lexicographic order chronological.
func isoChangeStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000") + "Z"
}
