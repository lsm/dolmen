package store

import (
	"context"
	"time"
)

// Listen registration — slice 6b (r2). This file is the REGISTER half of
// §6.2/§9.3's atomic register-and-replay: one immediate write transaction
// fixes, as a single coordinated operation, the resume position P, the
// replay boundary R, the page chain, the feed's table-lifetime labels, and
// the standing resume cursor. The replay-page read, the live delivery, the
// registry join that orders them, and the loss-check baseline land in the
// following slices of the 6b stack; until then a drained registration
// reports done and notify is never invoked.

// Listen implements the engine-declared notification capability (§6.2, §9.3):
// registration fixes the replay boundary and the feed's labels as ONE
// coordinated operation, the replay pages out through ChangeReplay.Next, and
// — once the live half of the 6b stack lands — the session pushes live
// records through notify after the replay drains. closed fires at most once,
// only when the ENGINE ends the session, never for a caller-initiated
// cancel; the returned cancel is idempotent.
//
// TODO(6b-live): the live half — the registry join, the fill/drain pumps,
// notify delivery, and the overflow / lifetime teaching closes — is 6b's
// next slice; this body serves the replay half only.
// TODO(9d): while auth is off the SSE handler passes a nil liveAuthz (no
// per-event filter); the admission rules are live the moment a slice
// wires a real re-resolver in.
func (s *Store) Listen(ctx context.Context, nsName, table string, from Cursor, nsGen [16]byte, liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool), notify func(ChangeRecord), closed func(cause error)) (*ChangeReplay, func(), error) {
	if notify == nil {
		return nil, nil, invalidf("listen: notify callback is required")
	}
	// nsCtx, not ns: registration may queue behind s.mu — a DropNamespace
	// draining its in-flight connections holds it, and that drain is
	// explicitly unbounded — so the caller's context must bound both the
	// mutex wait and a first-open initialization (the same rule
	// ChangesSince follows).
	n, err := s.nsCtx(ctx, nsName)
	if err != nil {
		return nil, nil, err
	}
	sess := &listenSession{
		s: s, n: n, nsName: nsName, table: table, nsGen: nsGen,
		liveAuthz: liveAuthz, notify: notify, closedFn: closed,
		flight: make(chan struct{}, 1),
	}

	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	// now is captured AFTER the transaction is held: BeginTx can queue
	// behind the namespace's single write connection for as long as its
	// current holder takes, and a timestamp from before that wait would
	// mint the standing cursor with issuance and chain start already stale
	// — or refresh a presented token against a time from before it truly
	// expired — leaving the returned resume cursor dead on arrival.
	now := time.Now()

	// The table feed's target, fixed at registration: the table exists now,
	// and the labels pin WHICH lifetime the feed follows for its whole life —
	// a same-named successor is a different feed, and the session ends rather
	// than silently narrowing to it.
	if table != "" {
		var ferr error
		if sess.feed, ferr = changeFeedOf(ctx, tx, nsName, table); ferr != nil {
			return nil, nil, ferr
		}
	}

	// The resume position and its page chain — ChangesSince's exact rules
	// (zero = current head, "begin" = the retained-history boundary, anything
	// else an opaque token with feed binding and retention enforced before
	// the position is honored).
	switch {
	case from == "":
		if sess.position, err = changeHead(ctx, tx); err != nil {
			return nil, nil, err
		}
		sess.chain = newCursorChain(now, sess.position)
	case from == CursorBegin:
		if sess.position, err = changeBegin(ctx, tx, now, s.changeRetention); err != nil {
			return nil, nil, err
		}
		sess.chain = newCursorChain(now, sess.position)
	default:
		var row cursorRow
		if row, err = resolveCursorToken(ctx, tx, now, s.changeRetention, from, table); err != nil {
			return nil, nil, err
		}
		sess.position = row.Position
		sess.chain = &cursorChain{ID: row.ChainID, Origin: row.ChainOrigin, Start: row.ChainStart}
		// The presented token is RETAINED, not re-minted: it resolves at P,
		// and resolve just refreshed its deadline, so it resumes exactly as
		// a fresh one would (the empty-page echo's rule, §9.3).
		sess.nextCursor = from
	}

	// The boundary: the replay is (P, R], bounded even when R is 0 (the
	// empty log's head — a real bound, never "unbounded"), and everything
	// after R is the live half's to deliver. The write lock this
	// transaction holds (the cursor mint/refresh above) means no writer can
	// interleave between the head reads and the commit — R is the head of a
	// serial-observability point (§0.6).
	if sess.boundary, err = changeHead(ctx, tx); err != nil {
		return nil, nil, err
	}
	// The standing resume cursor is fixed HERE, at registration, so a
	// session that ends before its first Next — cancelled by its caller, or
	// ended by the engine once the live half lands (an overflow of its own
	// traffic) — still hands back a boundary the caller can resume from
	// EXACTLY where registration stood: a terminal reporting an empty
	// cursor would send the reconnecting client to the current head and
	// skip every undelivered commit after registration. Bare and begin
	// starts mint one at the resume position (the undelivered backlog
	// stays behind the cursor); a presented token was retained above.
	if sess.nextCursor == "" {
		var terr error
		if sess.nextCursor, terr = mintCursorToken(ctx, tx, now, sess.position, table, sess.chain); terr != nil {
			return nil, nil, terr
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return &ChangeReplay{Next: sess.next, Resume: sess.cursor}, sess.cancel, nil
}

