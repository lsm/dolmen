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
	// ErrCursorExpired is the engine's shape of §9.3's beyond-retention
	// error: the presented token is unknown, pruned, past its own deadline,
	// or past its chain's absolute cap — either way it names no position. The
	// op layer maps it to the explicit teaching error naming the catch-up
	// path; whether it fires is a function of the client's own token age
	// alone, never of what the log contains.
	ErrCursorExpired = errors.New("cursor token is unknown or past retention")

	// ErrCursorCrossFeed: the token was minted on a different feed (another
	// table's, or the unfiltered namespace feed) than the one it was resolved
	// against. Cross-feed reuse is rejected, never honored as a position —
	// honoring it would silently skip the target feed's events behind the
	// foreign feed's cursor (§9.3).
	ErrCursorCrossFeed = errors.New("cursor token belongs to a different feed")
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
// still reach satisfies M ≥ T−R, and the boundary is the FIRST record no
// older than R before the call: it and everything after it is readable
// through the whole chain; anything older is outside every replay guarantee
// and skipped, never replayed into a chain that pruning could then break
// mid-page.
//
// The boundary is derived from the earliest in-window record (its seq minus
// one), never from the out-of-window prefix: `at` is a persisted wall-clock
// stamp and can move backward between commits (a clock step, a skewed
// writer), so out-of-window records need not form a prefix — a boundary
// placed past a MAX of them could sit beyond an earlier in-window record and
// silently skip guaranteed history. On an ordered log the two formulations
// pick the same position; under disorder this one can only over-deliver
// (interleaved out-of-window records ride the chain, retained through its
// deadline like every other record past the origin), never skip.
//
// With retention disabled (R = 0) records never age out and the headroom
// argument is vacuous — the formulas must NOT apply, or M ≥ T skips retained
// history: the boundary is the oldest retained record itself (§9.3).
//
// When no record qualifies — an empty log, or everything older than the
// window — the boundary is the current head: exactly the bare-start
// semantics, nothing retained is readable and the chain delivers future
// commits only.
func changeBegin(ctx context.Context, db rowQuerier, now time.Time, retention time.Duration) (int64, error) {
	// R = 0: oldest retained record; the chain resumes after it — position
	// seq−1, which a never-pruned log makes 0.
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
	// R > 0: first record inside the window, by seq — the earliest sequence
	// whose stamp qualifies, not the newest out-of-window one (see above).
	var first sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MIN(seq) FROM _dolmen_changes WHERE at >= ?`,
		isoChangeStamp(now.Add(-retention))).Scan(&first); err != nil {
		return 0, err
	}
	if !first.Valid {
		// Nothing retained is within the window — the bare-start position.
		return changeHead(ctx, db)
	}
	return first.Int64 - 1, nil
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

// newCursorChain mints a NEW chain's identity, rooted at the resume position:
// Origin carries the begin-boundary (or head) semantics every token minted on
// the chain inherits, and Start anchors the absolute cap chain_start + 2R
// (§9.3). ChangesSince builds the chain up front so every mint of the call —
// each record's cursor and the next-page token — rides one identity; nil-chain
// mints (5b helpers, tests) construct their own through this same path.
func newCursorChain(now time.Time, position int64) *cursorChain {
	var cid [cursorTokenBytes]byte
	if _, err := rand.Read(cid[:]); err != nil {
		// Unreachable in practice (a broken entropy source fails the token
		// mint first); an empty id still groups the chain, only less
		// observably.
		return &cursorChain{Origin: position, Start: now.UnixMilli()}
	}
	return &cursorChain{ID: hex.EncodeToString(cid[:]), Origin: position, Start: now.UnixMilli()}
}

// resolveCursorToken maps an opaque token back to its position and chain,
// enforcing §9.3's validity rules BEFORE the position is honored:
//
//   - Feed binding: the stored feed_table must equal the caller's table
//     selector (” = the unfiltered namespace feed). Anything else is
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
// chain_start is immutable per chain. The refresh is MONOTONIC (MAX of the
// stored and supplied stamps): duplicate requests resolving the same token
// concurrently capture different nows, and an older now landing last must
// not move the refreshed deadline backward — a retry near the retention
// boundary would otherwise expire earlier than promised. The UPDATE is also
// the atomic existence re-check: on the shared rw pool the SELECT and the
// UPDATE are separate statements, and a concurrent pruneChanges may delete
// the token — and the records it can still reach — between them. An UPDATE
// that matched zero rows means exactly that: the position must not be
// honored, or replay would be shortened under a live cursor (§9.3).
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

// pruneChanges applies §9.3's retention, TIME-based only: never a record
// count or byte cap, which would let foreign commits evict a scoped reader's
// cursor while that reader has received nothing visible and turn an
// otherwise-empty resume into an error that reveals hidden traffic. Retention
// 0 disables pruning entirely — records and tokens accumulate.
//
// Tokens are pruned once past their own deadline (issued_at + R) or their
// chain's absolute cap (chain_start + 2R), whichever comes first. The two
// predicates run as separate sargable DELETEs — issued_at < now−R and
// chain_start < now−2R, each served by its index — because pruning rides
// every changes_since call under the namespace's write lock, and a token
// table that accumulates a row per poll for a whole retention window must
// not be full-scanned per call (the arithmetic `col + R < now` forms would
// defeat the indexes; the subtracted-cutoff forms are the same inequality).
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
// by the scalar subquery (the chain_origin index serves MIN as a leftmost-key
// seek — a correlated per-record EXISTS would rescan the token table for
// every candidate record, and both tables grow for the whole retention
// window). With no surviving chains the minimum is NULL and every
// age-eligible record goes.
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

// isoChangeStamp renders t exactly as the log's `at` column stamps it —
// SQLite's '%Y-%m-%dT%H:%M:%fZ': UTC with a fixed 3-digit millisecond field —
// so boundary and pruning cutoffs compare against `at` as plain strings:
// identical shapes make lexicographic order chronological.
func isoChangeStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

// ---------------------------------------------------------------------------
// ChangesSince — slice 5c, the replay op assembled from the 5b helpers.

// Page bounds for the change-log reads (§9.3): limit default 100, max 1000 —
// pinned like search pagination so the replay-bounding rules are enforceable
// against a bounded page and conforming adapters agree on the bounds. The op
// layer REJECTS outside 1–1000 (invalid_request); the engine clamps, per
// Page's conventions.
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

// loggedChange is one scanned change-log row: the public record plus the seq
// its cursor must be minted at — the position never crosses the seam, so it
// lives beside the record only between the page read and the token mints.
type loggedChange struct {
	rec ChangeRecord
	seq int64
}

// changeFeed is one table feed's identity: the table plus the lifetime
// labels a record must carry to belong to the feed — the nsgen stamped at
// mint and the table's drop generation. A ChangesSince call reads the
// labels fresh per page; a Listen session fixes them at registration (a
// same-named successor is a different feed, §9.3). A nil *changeFeed is the
// namespace-wide feed: no table filter, every lifetime included.
type changeFeed struct {
	table   string
	nsgen   [16]byte
	dropGen int64
}

// changeFeedOf validates a table feed's target inside tx and returns its
// CURRENT lifetime labels, read in the caller's snapshot so a
// drop-and-recreate committing elsewhere cannot mix a predecessor's records
// into the successor's feed. A missing table is ErrNotFound — the §6.2
// no-implicit-anything rule.
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

// changePageSQL builds the change-log page read: records in (from, to] in
// seq order (to <= 0: unbounded), restricted to one table lifetime's records
// when feed is non-nil, capped at limit.
func changePageSQL(from, to int64, limit int, feed *changeFeed) (string, []any) {
	q := `SELECT seq, table_name, row_id, kind, owner, nsgen, drop_gen FROM _dolmen_changes WHERE seq > ?`
	args := []any{from}
	if to > 0 {
		q += ` AND seq <= ?`
		args = append(args, to)
	}
	if feed != nil {
		q += ` AND table_name = ? AND drop_gen = ? AND nsgen = ?`
		args = append(args, feed.table, feed.dropGen, feed.nsgen[:])
	}
	return q + ` ORDER BY seq LIMIT ?`, append(args, limit)
}

// scanChangePage materializes one bounded page of change records. The buffer
// is bounded by the caller's LIMIT — the same contract that bounds every
// public page (§9.3) — and each row's nsgen label is validated on decode: a
// corrupt blob must never silently masquerade as a lifetime key.
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

// mintChangeCursors mints the page's cursors: one fresh token per record at
// the record's position — a client persisting its last-delivered cursor
// resumes exactly after it, and consecutive visible records yield unrelated
// tokens (§9.3's opacity rules) — then the next-page token, all on the one
// chain.
//
// The next-page cursor. An EMPTY page re-presents the caller's own token
// (slice 5d's rule): the position did not move, so nothing new is issuable,
// and minting fresh randomness per poll would hand a 250 ms wait_for tick a
// durable row every time — hundreds of thousands per idle waiter per day.
// Resolve already refreshed the token's deadline, so the echoed token
// resumes exactly as a fresh one would. Bare and begin starts still mint
// (the response must carry a real head/boundary token the caller did not
// present), and fresh randomness stays the rule for every NEW position —
// per-record cursors and non-empty pages. Callers that present no client
// token (Listen's session halves, which page on demand rather than on a
// poll tick) pass the empty cursor and always mint.
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

// ChangesSince is the scoped replay read over the engine-owned durable change
// log (§6.2, §9.3): records in cursor (seq) order after the resume position,
// as one bounded page plus the next cursor. Everything the call touches —
// token resolution (which refreshes the presented token's deadline), the page
// read, the per-record and next-page token mints, and opportunistic retention
// pruning — runs inside ONE write transaction, the namespace's rw pool with
// its immediate lock: the page and its cursors commit together (a crash never
// returns records whose cursors were never stored), no concurrent prune can
// interleave between the resolve and the mints (the 5b helpers each defend
// against that separately; one transaction removes the windows entirely), and
// the read sees exactly one log snapshot.
//
// The resume position comes from `from` (§9.3's pinned forms): the zero cursor
// is the CURRENT HEAD — a fresh subscriber wants future events only, so the
// first page is empty by construction and carries the head cursor as its
// next_cursor; the CursorBegin sentinel starts at the retained-history
// boundary (changeBegin); anything else is an opaque token resolved through
// the mapping table, with feed binding, own deadline, and chain cap enforced
// before the position is honored (ErrCursorExpired / ErrCursorCrossFeed).
// A fresh start builds a NEW page chain rooted at its position, so every
// token this call mints inherits the same origin (the boundary/head
// semantics) and cap anchor.
//
// table != "" selects the table feed: only records of the table's CURRENT
// lifetime are delivered — the per-record lifetime labels (nsgen + drop_gen,
// §3.4) compared against the table's current generation, so a caller on a
// recreated same-named successor never replays the predecessor's records,
// even from a cursor minted before the drop. The table must exist
// (ErrNotFound, the §6.2 no-implicit-anything rule). table == "" is the
// namespace feed: the namespace's own recorded history, every table
// lifetime included — its readers held namespace read throughout.
//
// Each returned record carries its OWN cursor — a fresh token at the record's
// position, so a client persisting its last-delivered cursor resumes exactly
// after it, and consecutive visible records yield unrelated tokens (the
// opacity rules: a filtered feed's gaps stay invisible). next_cursor maps to
// the last delivered record's position, or to the resume position on an empty
// page. Public projections expose cursor/table/row_id/kind only; Owner and
// Lifetime are internal metadata for the authz-era visible-set filtering.
//
// TODO(8c): nsGen is ignored while auth is off — slice 8c verifies the
// namespace-lifetime guard atomically with the read, exactly like Query.
// TODO(9d): scope and scopeIncarnation are ignored while auth is off —
// slice 9d filters records through the caller's visible set via the Owner
// label and binds the scope's incarnation.
func (s *Store) ChangesSince(ctx context.Context, nsName, table string, from Cursor, nsGen [16]byte, scope *RowScope, scopeIncarnation Incarnation, page Page) ([]ChangeRecord, Cursor, error) {
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

	// The table feed's current-lifetime filter: the table must exist, and the
	// labels a record must carry to belong to this lifetime are read in the
	// same snapshot as the page — a drop-and-recreate committing elsewhere
	// cannot mix a predecessor's records into the successor's feed.
	var feed *changeFeed
	if table != "" {
		var ferr error
		if feed, ferr = changeFeedOf(ctx, tx, nsName, table); ferr != nil {
			return nil, "", ferr
		}
	}

	// The resume position and its page chain.
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

	// The bounded page, then its cursors — assembled from the shared helpers
	// Listen's session halves use too, so every replay path reads and mints
	// identically. The page is capped at MaxChangesPageLimit, so the buffer
	// is bounded by the same contract that bounds the response.
	limit := changesPageLimit(page.Limit)
	query, args := changePageSQL(position, 0, limit, feed)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	scanned, err := scanChangePage(rows)
	rows.Close()
	if err != nil {
		return nil, "", err
	}

	// Mint the cursors: one per record at the record's position, then the
	// next-page token — all on the one chain, so a client may persist either
	// a record's cursor or next_cursor and resume gap-free. The presented
	// cursor rides along for the empty-page echo (mintChangeCursors).
	records, next, err := mintChangeCursors(ctx, tx, now, scanned, position, from, table, chain)
	if err != nil {
		return nil, "", err
	}

	// Retention moves with the read: by the time this runs, the call's own
	// chain row is in the mapping table, so pruning sees it and keeps every
	// record it can still reach (pruneChanges consults the durable chain
	// rows, never age alone).
	if err := pruneChanges(ctx, tx, now, s.changeRetention); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return records, next, nil
}
