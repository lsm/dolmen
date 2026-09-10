package store

import (
	"context"
	"time"
)

// Replay-page reading — slice 6b (r3). This file pages the registration
// range (P, R] out through ChangeReplay.Next: one bounded page per call,
// records in cursor order, each carrying a cursor minted in its own
// transaction, and the done contract of §6.2 — the page carrying the final
// records reports done=false, the following call reports done=true with no
// records, the registration boundary. The loss checks (prune-vs-promise),
// the per-record admission gate, and the live half land in the following
// slices; until then the page exposes every scanned record.

// next pages the replay half and owns the session-state publish: page
// progress is applied HERE, under one lock, only after the omission
// decision is final — publishing inside page left a window where a
// concurrent Resume() observed the post-page cursor and the omission then
// rolled it back (a returned cursor cannot be retracted). A page whose
// session died mid-flight keeps the PRE-page standing cursor: the page is
// omitted, and Resume must not teach a position past records the caller
// never received. A page that did not complete (a canceled Next context,
// a mint failure) publishes nothing either — its progress is a zero value,
// and publishing it would wipe the standing cursor with "" and teach a
// later Resume a bare-head restart that skips the entire undelivered
// backlog. State stays at the last completed page.
//
// A dedicated single-flight mutex serializes concurrent Next calls for the
// page AND the publish as one unit: two interleaved callers would both
// scan from the same sess.position (duplicate records), and a slower
// earlier call could publish its position after a later one's, regressing
// Resume. nextMu sits OUTSIDE mu so the page's database work holds only
// the flight lock — sess.mu remains the short-held state lock.
func (sess *listenSession) next(ctx context.Context) ([]ChangeRecord, Cursor, bool, error) {
	sess.nextMu.Lock()
	defer sess.nextMu.Unlock()
	sess.mu.Lock()
	resume := sess.nextCursor // the PRE-page boundary: what an omitted page must hand back
	sess.mu.Unlock()
	records, next, done, progress, err := sess.page(ctx)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.dead && len(records) > 0 {
		// The omitted page's own cursor points PAST records the caller never
		// receives — handing it back would teach a resume that skips them.
		// The pre-page boundary is the honest position.
		return nil, resume, true, nil
	}
	if err != nil || progress.next == "" {
		return records, next, done, err
	}
	sess.replayExhausted = progress.short
	if progress.scannedLast > 0 {
		sess.position = progress.scannedLast
	}
	sess.nextCursor = progress.next
	sess.outstanding -= progress.consumed
	return records, next, done, err
}

// page reads one bounded page of (position, boundary] under the
// registration labels — the labels and the boundary are registration-fixed,
// so a drop or recreate committing mid-replay can neither narrow this range
// nor mix a successor's records into it. The boundary is ALWAYS bounded,
// even when it is 0 (the empty log's head): a commit landing before the
// caller's first Next is the live half's to deliver, and an unbounded read
// here would replay it too (§6.2's exactly-once).
func (sess *listenSession) page(ctx context.Context) ([]ChangeRecord, Cursor, bool, pageProgress, error) {
	sess.mu.Lock()
	dead, exhausted := sess.dead, sess.replayExhausted
	sess.mu.Unlock()
	if dead || exhausted {
		return nil, sess.cursor(), true, pageProgress{}, nil
	}
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", false, pageProgress{}, err
	}
	// The namespace's write pool is a single connection: a transaction that
	// returns without committing or rolling back holds it, and every later
	// write or replay against the namespace blocks indefinitely. The defer
	// makes every path below release it (a no-op after Commit).
	defer tx.Rollback()
	boundary := sess.boundary
	query, args := changePageSQL(sess.position, &boundary, MaxChangesPageLimit, sess.feed)
	rows, qerr := tx.QueryContext(ctx, query, args...)
	var scanned []loggedChange
	if qerr == nil {
		scanned, qerr = scanChangePage(rows)
		rows.Close()
	}
	// The consumed count is the SCAN ITSELF — every scanned feed row is
	// consumed exactly once; the outstanding baseline is fully verified
	// before the scan by the loss-check slice's entry check, so no
	// per-page recount of the tail is needed here.
	consumed := int64(len(scanned))
	if qerr == nil {
		qerr = tx.Commit()
	}
	if qerr != nil {
		return nil, "", false, pageProgress{}, qerr
	}

	var mLast int64
	if len(scanned) > 0 {
		mLast = scanned[len(scanned)-1].seq
	}
	records, next, merr := sess.mint(ctx, scanned, sess.position, mLast, len(scanned))
	if merr != nil {
		return nil, "", false, pageProgress{}, merr
	}

	// Exhaustion keys on the SCANNED range, never on the admitted page: a
	// full page whose every record the authorization filtered out is not the
	// boundary — visible records may still sit before it, and ending replay
	// on an empty admitted page would strand them behind a page the caller
	// will never turn.
	short := len(scanned) < MaxChangesPageLimit
	// Page progress stays UNPUBLISHED here — next() applies it after the
	// omission decision is final.
	progress := pageProgress{short: short, next: next, consumed: consumed, scannedLast: mLast, scannedCount: len(scanned)}
	// A short scanned page with nothing to expose IS the boundary — the
	// replay is done, and the live half (a later slice) takes over from
	// here. A short page with records reports done=false (the final
	// records; the FOLLOWING call reports the boundary, §6.2), and a full
	// page pages on.
	if short && len(records) == 0 {
		return nil, next, true, progress, nil
	}
	return records, next, false, progress, nil
}

// pageProgress is a completed page's unpublished session advance, applied
// by next() only when the page is delivered.
type pageProgress struct {
	short        bool
	scannedLast  int64
	scannedCount int
	next         Cursor
	consumed     int64
}

// mint writes one transaction of cursor mints plus opportunistic retention
// pruning — the shared tail of both session halves, matching ChangesSince's
// mint-then-prune shape so every replay path refreshes chains identically.
// The next-page token is the replay's standing resume cursor.
func (sess *listenSession) mint(ctx context.Context, admitted []loggedChange, resume int64, scannedLast int64, scannedCount int) ([]ChangeRecord, Cursor, error) {
	tx, err := sess.n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	// now is captured AFTER the transaction is held — the same rule as
	// registration: BeginTx can queue behind the namespace's single write
	// connection, and stamps from before the wait would hand the caller
	// cursors whose issuance is already expired.
	now := time.Now()
	records, next, err := mintChangeCursors(ctx, tx, now, admitted, resume, "", sess.table, sess.chain)
	if err != nil {
		return nil, "", err
	}
	if err := pruneChanges(ctx, tx, now, sess.s.changeRetention); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return records, next, nil
}
