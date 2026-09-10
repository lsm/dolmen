package store

import (
	"context"
	"sync"
	"time"
)

// Listen registration — slice 6b (r2; the registry join is r6b's). This
// file is the REGISTER half of §6.2/§9.3's atomic register-and-replay:
// one immediate write transaction fixes, as a single coordinated
// operation, the resume position P, the replay boundary R, the page
// chain, the feed's table-lifetime labels, and the standing resume
// cursor — and the session joins the commit registry BEFORE that
// transaction, so a record can only take seq > R by committing after
// the join (the register-and-replay ordering; the replay-page read and
// loss baseline landed with r3/r4). The fill pump the wake feeds, the
// drain that delivers its queue through notify, and the queue's bound
// land in the following slices; until then a drained registration
// reports done and notify is never invoked.

// Listen implements the engine-declared notification capability (§6.2, §9.3):
// registration fixes the replay boundary and the feed's labels as ONE
// coordinated operation, the replay pages out through ChangeReplay.Next, and
// — once the live half of the 6b stack lands — the session pushes live
// records through notify after the replay drains. closed fires at most once,
// only when the ENGINE ends the session, never for a caller-initiated
// cancel; the returned cancel is idempotent.
//
// TODO(6b-live): the remaining live half — the fill pump the wake feeds,
// the drain that delivers its queue through notify, the queue bound and
// its overflow teaching close, and the admission gate — is 6b's next
// slice.
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
	// The live half's signal, wired at construction so every end (caller
	// cancel included) can broadcast a parked pump out of cond.Wait.
	sess.cond = sync.NewCond(&sess.mu)
	// The registry join comes BEFORE the boundary transaction — the
	// register-and-replay ordering (§6.2): the boundary read runs under
	// the namespace's write lock, so any commit that takes a seq AFTER
	// the boundary necessarily lands after this join — its wake finds the
	// session already registered, and replay (P, R] and live > R are
	// disjoint by construction. Joining after the transaction would leave
	// a commit that raced it unwitnessed by both halves. A registration
	// that fails past this point takes the entry back out on its way down.
	sess.unregister = s.onCommit(nsName, sess.wake)
	committed := false
	defer func() {
		if !committed {
			sess.unregister()
		}
	}()

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
	// The loss-check baseline, in the same snapshot: the feed's rows the
	// log ACTUALLY retains in (P, R] — not the span arithmetic, because
	// pruning deletes by age and clock-stepped at stamps can leave holes a
	// begin registration legitimately replays around (changeBegin selects
	// the first retained record, whatever sits missing beside it), and not
	// the whole namespace's, because a table feed's promise is only its own
	// records. Only rows removed AFTER this count are the session's to
	// lose.
	//
	// TODO(9d): the count covers every FEED row, including rows a scoped
	// viewer can never receive — a hidden aged row deleted mid-session
	// currently reads as loss even when the visible backlog is intact
	// (§9.3 says foreign traffic must never evict a scoped reader).
	// Filtering the count needs a side-effect-free view of the current
	// scope; re-invoking liveAuthz is NOT it — the admission callback is
	// per-event by contract (§6.2), and extra invocations from the counting
	// path perturb re-resolvers that sequence or audit on calls. The 9d
	// scope-resolver contract should expose the predicate; until then, with
	// auth off, liveAuthz is nil everywhere but tests.
	if sess.position < sess.boundary {
		cq, cargs := changeCountSQL(sess.position, sess.boundary, sess.feed)
		if err = tx.QueryRowContext(ctx, cq, cargs...).Scan(&sess.outstanding); err != nil {
			return nil, nil, err
		}
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
	committed = true // the session owns its registry entry now; cancel removes it
	return &ChangeReplay{Next: sess.next, Resume: sess.cursor}, sess.cancel, nil
}
