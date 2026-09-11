package store

import (
	"context"
	"database/sql"
	"fmt"
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
// the flight lock — sess.mu remains the short-held state lock — and the
// serialization wait is CONTEXT-AWARE: a caller blocked behind another
// Next's database work returns when ITS OWN context cancels rather than
// waiting out the holder (a Background-context holder stuck behind the
// namespace's single write connection must not strand a deadlined caller).
func (sess *listenSession) next(ctx context.Context) ([]ChangeRecord, Cursor, bool, error) {
	if err := sess.lockNext(ctx); err != nil {
		return nil, "", false, err
	}
	defer func() { <-sess.flight }()
	sess.mu.Lock()
	resume := sess.nextCursor // the PRE-page boundary: what an omitted page must hand back
	// Mark the page IN FLIGHT for its whole span — read, mint: the drainer
	// must not deliver past the boundary while the final page is still being
	// decided (and, once the parked-close machinery lands, a terminal must
	// not cut past it either — §6.2's exposure rule). The clear rides a
	// DEFER so a panicking page cannot strand the flag and wedge the
	// drainer behind it.
	sess.replayActive = true
	sess.mu.Unlock()
	records, next, done, progress, err := func() (a []ChangeRecord, b Cursor, c bool, d pageProgress, e error) {
		defer func() {
			sess.mu.Lock()
			sess.replayActive = false
			sess.cond.Broadcast()
			sess.mu.Unlock()
		}()
		return sess.page(ctx)
	}()
	sess.mu.Lock()
	defer sess.mu.Unlock()
	// done with no error — from ANY path, including the exhausted-entry
	// boundary call that carries zero progress — means the replay is over:
	// release the drainer HERE, inside the caller's own final word (never
	// mid-page), before the branches below decide what to publish.
	if done && err == nil {
		sess.replayDone = true
		sess.cond.Broadcast()
	}
	if sess.dead && len(records) > 0 {
		// The omitted page's own cursor points PAST records the caller never
		// receives — handing it back would teach a resume that skips them.
		// The pre-page boundary is the honest position. The session is over:
		// release any drainer the same way.
		sess.replayDone = true
		sess.cond.Broadcast()
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
	// A replay must never SILENTLY shorten — but it must equally accept what
	// registration found. The loss check is a BASELINE, not span arithmetic:
	// registration counted the rows the log actually retains in (P, R]
	// (holes included — pruning deletes by age, and clock-stepped at stamps
	// leave gaps that a begin registration legitimately replays around),
	// and each page verifies the log still holds exactly that many rows in
	// its outstanding range BEFORE scanning (verifyRetained — the full
	// tail, never a span the scan itself would make tautological). Loss is
	// the cursor-expiry teaching error for the caller to reconnect from its
	// last delivered cursor — never a short page reported as done, which
	// would silently omit records the boundary promised.
	if err := sess.verifyRetained(ctx, tx, sess.position, sess.boundary, sess.outstanding); err != nil {
		return nil, "", false, pageProgress{}, err
	}
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
	var admitted []loggedChange
	for _, lc := range scanned {
		vis, rev := sess.admit(lc.rec)
		if rev {
			sess.end(ErrListenRevoked)
			return nil, sess.cursor(), true, pageProgress{}, nil
		}
		if vis {
			admitted = append(admitted, lc)
		}
	}
	records, next, merr := sess.mint(ctx, admitted, sess.position, mLast, len(scanned))
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
	progress := pageProgress{short: short, next: next, consumed: consumed, scannedLast: mLast}
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
	short       bool
	scannedLast int64
	next        Cursor
	consumed    int64
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
	// The consumed span REVALIDATED here, under the mint's own write lock:
	// the page's entry check verified the full tail in its read
	// transaction, but the mint runs in a LATER one — a queued writer's
	// prune can land in the gap and delete rows this page scanned. Minting
	// tokens for deleted positions would hand the caller records whose
	// cursors resolve over a gutted log; the recount, bounded to exactly
	// what the page consumed, fails loudly instead.
	if scannedCount > 0 {
		if err := sess.verifyRetained(ctx, tx, resume, scannedLast, int64(scannedCount)); err != nil {
			return nil, "", err
		}
	}
	records, next, err := mintChangeCursors(ctx, tx, now, admitted, resume, "", sess.table, sess.chainFor(now, resume))
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

// lockNext acquires the single-flight lock or fails with the context's
// error. The flight lock is a BUFFERED CHANNEL semaphore, not a mutex: a
// channel receive is cancellable, so a caller blocked behind another
// Next's database work returns when ITS OWN context cancels — without
// spawning a goroutine per waiter (a mutex has no cancellable Lock; the
// watcher-goroutine workaround leaked two goroutines per canceled caller
// until the holder released).
func (sess *listenSession) lockNext(ctx context.Context) error {
	if ctx == nil || ctx.Done() == nil {
		sess.flight <- flightToken
		return nil
	}
	select {
	case sess.flight <- flightToken:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// flightToken is the single flight permit (a value, not a constant: struct{}{} is not a constant expression).
// chainFor follows below (r5).
var flightToken = struct{}{}

// chainFor returns the chain this mint rides. A token's life is bounded
// twice — its own issued_at + R, and its chain's absolute cap
// chain_start + 2R (§9.3) — so a mint in the chain's SECOND R stamps
// tokens whose chain cap lands before their own expiry: valid at the
// check, dead shortly after (and the mint's own work — up to a page of
// token inserts, the prune, the commit — can itself cross the cap between
// the check and the insertions). The rotation boundary is therefore the
// chain's FIRST R: every mint rides a chain with at least R of headroom,
// exactly the token's own TTL, so the chain cap never expires a token
// before its own expiry does. A fresh chain roots at the current position,
// exactly the chain a client resubscribing from here starts itself: the
// old backlog the cap exists to bound stays bounded, and every delivered
// cursor stays resolvable for its full promised life. Retention 0 has no
// caps and never rotates.
//
// While the replay is still draining, a rotation's origin floors at the
// replay's outstanding position: a chain rooted at a later position would
// let the same transaction's prune delete age-eligible records the replay
// has not yet paged — silently shortening the gap-free resume the chain
// exists to protect. Once the replay is exhausted the floor lifts; nothing
// below the current position remains to protect.
func (sess *listenSession) chainFor(now time.Time, resume int64) *cursorChain {
	if sess.s.changeRetention <= 0 {
		return sess.chain
	}
	rms := int64(sess.s.changeRetention / time.Millisecond)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if now.UnixMilli() < sess.chain.Start+rms {
		return sess.chain
	}
	origin := resume
	if !sess.replayExhausted && sess.position < origin {
		origin = sess.position
	}
	if len(sess.queue) > 0 && sess.queue[0].seq < origin {
		origin = sess.queue[0].seq
	}
	sess.chain = newCursorChain(now, origin)
	return sess.chain
}

// verifyRetained recounts, in one query, whether the log still holds
// what was promised (the registration baseline, or a page's consumed
// span). Three facts about its cost and design, recorded where the next
// reader lives:
//
//  1. COST — the count is O(rows in (from, to]) and runs in the
//     namespace's single-connection write transaction: an entry check
//     walks the full outstanding tail per page (quadratic over the
//     replay), and the registration baseline adds one more full scan.
//     Preview posture: correctness over scan cost (the engineering-preview
//     goal defers performance), and the quadratic term is bounded by the
//     retention window and page count.
//
//  2. WHY NOT BOUNDED — recounting only a span the caller is about to
//     scan is TAUTOLOGICAL for deletions that predate the scan (scan and
//     count see the same already-pruned log), so a pre-scan deletion in
//     the unconsumed tail goes undetected until pages have served records
//     past the hole. The bounded variant was implemented and reviewed on
//     the predecessor stack and proved unsound (interior-hole and
//     exact-tile omissions); only the full tail (or a consumed span
//     revalidated in a LATER transaction than its scan — the mint's
//     recheck) detects loss soundly on this storage shape.
//
//  3. THE O(1) PATH — a deletion-generation column on _dolmen_changes
//     (bumped by pruneChanges) would make loss detection a single
//     generation comparison per page: registration records the
//     generation, each page verifies it unchanged. A schema change, so a
//     future slice of its own (demand-gated, recorded on issue #189) —
//     not a correctness hotfix here.
func (sess *listenSession) verifyRetained(ctx context.Context, tx *sql.Tx, from, to, promised int64) error {
	if from >= to || promised == 0 {
		return nil
	}
	cq, cargs := changeCountSQL(from, to, sess.feed)
	var kept int64
	if err := tx.QueryRowContext(ctx, cq, cargs...).Scan(&kept); err != nil {
		return err
	}
	if kept != promised {
		return fmt.Errorf("listen replay: %w", ErrCursorExpired)
	}
	return nil
}
