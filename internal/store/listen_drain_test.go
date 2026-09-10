package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Slice 6b (the drain): r7a landed the delivery mint; this slice lands
// the loop that drives it. The pins: a live record mints a resolvable
// token (at its position), a record the log no longer holds fails loudly
// rather than minting a skipping cursor, the replay's boundary call gates
// every live delivery (nothing live before the replay has fully
// reported, in order after it), the mint failure closes the session with
// the teaching expiry, and a panicking notify ends the session instead
// of the process.

// TestListenMintOneMintsAtPosition: the delivery token resolves at the
// delivered record's position — the reconnect point the client needs.
func TestListenMintOneMintsAtPosition(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)

	tok, err := sess.mintOne(context.Background(), 1)
	if err != nil {
		t.Fatalf("mintOne: %v", err)
	}
	if tok == "" {
		t.Fatal("mintOne returned an empty token")
	}
	tx, err := n.rw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("resolve tx: %v", err)
	}
	defer tx.Rollback()
	row, err := resolveCursorToken(context.Background(), tx, time.Now(), st.changeRetention, tok, sess.table)
	if err != nil {
		t.Fatalf("resolve delivered token: %v", err)
	}
	if row.Position != 1 {
		t.Fatalf("delivered token resolves at %d, want 1", row.Position)
	}
}

// TestListenMintOneMissingFailsLoudly: a record deleted between the fill's
// scan and the delivery (a concurrent pruner's window) mints NOTHING —
// the teaching expiry, never a token whose reconnect silently skips the
// deleted tail.
func TestListenMintOneMissingFailsLoudly(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)

	if _, err := sess.mintOne(context.Background(), 999); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("mintOne at a missing position = %v, want ErrCursorExpired", err)
	}
}

// TestListenDeliversLiveAfterReplayDrains pins the concatenation §6.2
// promises end to end: the replay pages the backlog out in cursor order,
// the boundary call reports done, and only then do live records arrive
// through notify — in commit order, each carrying a cursor minted at its
// delivery.
func TestListenDeliversLiveAfterReplayDrains(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2) // the backlog: the replay's

	got := make(chan ChangeRecord, 4)
	replay, cancel := listenOn(t, st, "", CursorBegin, func(r ChangeRecord) { got <- r }, nil)
	defer cancel()

	insertNotes(t, st, 2) // live: commits after registration, queued behind the replay

	replayed := drainReplay(t, replay)
	if ids := rowIDsOf(replayed); len(ids) != 2 {
		t.Fatalf("replay delivered rows %v, want the 2-row backlog", ids)
	}

	for i := 0; i < 2; i++ {
		select {
		case r := <-got:
			if r.RowID != int64(3+i) {
				t.Fatalf("live record %d arrived at row %d, want %d (commit order)", i, r.RowID, 3+i)
			}
			if r.Cursor == "" {
				t.Fatalf("live record %d carried no cursor — delivery mints one", i)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("live record %d never delivered after the boundary call", i)
		}
	}
}

// TestListenDrainWaitsForBoundaryCall: the queue may hold live commits,
// but delivery is gated on the replay's boundary call — the page carrying
// the final records does NOT release the drainer; only the completed
// done report does (§6.2's replay-then-live, as an ordering the caller
// can rely on).
func TestListenDrainWaitsForBoundaryCall(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2) // the backlog pages once, then reports the boundary

	got := make(chan ChangeRecord, 4)
	replay, cancel := listenOn(t, st, "", CursorBegin, func(r ChangeRecord) { got <- r }, nil)
	defer cancel()

	insertNotes(t, st, 2) // live commits: the fill queues them immediately

	noEarly := func(stage string) {
		select {
		case r := <-got:
			t.Fatalf("live record (row %d) delivered at %s — before the boundary call completed", r.RowID, stage)
		case <-time.After(200 * time.Millisecond):
		}
	}
	noEarly("rest")

	// The final-records page is NOT the boundary (§6.2: the FOLLOWING call
	// reports it) — delivery still waits.
	if _, _, done, err := replay.Next(context.Background()); err != nil || done {
		t.Fatalf("first Next = err %v, done %v, want nil error, done=false (the final records)", err, done)
	}
	noEarly("the final-records page")

	// The boundary call completes the replay: the drainer is released.
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}
	for i := 0; i < 2; i++ {
		select {
		case r := <-got:
			if r.RowID != int64(3+i) {
				t.Fatalf("live record %d arrived at row %d, want %d", i, r.RowID, 3+i)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("live record %d never delivered after the boundary call", i)
		}
	}
}

// TestListenDrainMintFailureCloses: a queued record whose log row a
// concurrent pruner deleted (mintOne's existence-check window) closes the
// session with the teaching expiry — never a delivered cursor whose
// reconnect silently skips the deleted tail, and never a hang.
func TestListenDrainMintFailureCloses(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 1)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	closedCause := make(chan error, 1)
	sess := testSession(func(cause error) { closedCause <- cause })
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)
	sess.queue = []loggedChange{{seq: 999}} // queued, but the log no longer holds it
	sess.replayDone = true                  // the boundary call has completed: the drainer runs
	sess.pumps.Add(1)
	go sess.drain()

	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrCursorExpired) {
			t.Fatalf("mint-failure close cause = %v, want the teaching ErrCursorExpired", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mint failure never closed the session")
	}
	sess.cancel() // idempotent teardown; also waits the drain out
}

// TestListenPanickingNotifyEndsSession: notify is caller code on the
// session's own goroutine — a panic there must end the session (the
// stream is unreliable; the durable log recovers) instead of taking the
// process down.
func TestListenPanickingNotifyEndsSession(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 1)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	closedCause := make(chan error, 1)
	sess := testSession(func(cause error) { closedCause <- cause })
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)
	sess.notify = func(ChangeRecord) { panic("boom") }
	sess.queue = []loggedChange{{seq: 1}}
	sess.replayDone = true
	sess.pumps.Add(1)
	go sess.drain()

	select {
	case cause := <-closedCause:
		if !strings.Contains(cause.Error(), "panicked") {
			t.Fatalf("panicking notify closed with %v, want the recovered panic", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("panicking notify never ended the session")
	}
	sess.cancel()
}

// TestListenNotifyDefersCancel: notify is caller code, and a natural
// callback is teardown — but cancel must not be called SYNCHRONOUSLY from
// inside it: the quiescence join would sit on the calling goroutine's
// own pump (no goroutine can wait itself out), and carving the delivery
// bracket out of the join would void external cancel's guarantee —
// deliveries are continuous on a busy stream (codex P1s on #228,
// threads r3984073040/r3984335493). The documented pattern is the
// deferred one: `go cancel()` tears the session down without the
// self-join, and no further record delivers after it lands.
func TestListenNotifyDefersCancel(t *testing.T) {
	st := openChangeStore(t)
	toredown := make(chan struct{})
	var unsubscribe sync.Once

	var cancel func()
	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {
		// The documented pattern: deferred, never synchronous — a second
		// record may still race the teardown landing, so the unsubscribe
		// arms once.
		unsubscribe.Do(func() { go cancel(); close(toredown) })
	}, nil)
	defer cancel()

	// The boundary call releases the drainer before any delivery.
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}
	insertNotes(t, st, 2)

	select {
	case <-toredown:
	case <-time.After(20 * time.Second):
		t.Fatal("no delivery arrived to unsubscribe on")
	}
	// The teardown landed: the replay reports done once the session ends,
	// and neither callback ever deadlocks.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, _, done, err := replay.Next(context.Background())
		if err != nil {
			t.Fatalf("post-teardown Next: %v", err)
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replay never reported done after the teardown")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestListenExternalCancelWaitsInFlightDelivery: the quiescence
// guarantee is the contract — an EXTERNAL cancel, called while a
// delivery is mid-notify, returns only after the callback has returned
// and the drain goroutine has exited: never delivery after cancel
// returned, never use-after-free of state the caller releases there
// (codex P1 on #228, thread r3984335493).
func TestListenExternalCancelWaitsInFlightDelivery(t *testing.T) {
	st := openChangeStore(t)

	inFlight := make(chan struct{}, 1)
	returned := make(chan struct{})
	release := make(chan struct{})
	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {
		inFlight <- struct{}{}
		<-release // park mid-notify: the external cancel must wait this out
		close(returned)
	}, nil)

	// The boundary call releases the drainer before any delivery.
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}
	insertNotes(t, st, 1)
	<-inFlight

	cancelReturned := make(chan struct{})
	go func() {
		cancel() // external: from outside the session's goroutines
		close(cancelReturned)
	}()
	select {
	case <-cancelReturned:
		t.Fatal("external cancel returned while the in-flight notify was still parked — the quiescence guarantee torn")
	case <-returned:
		t.Fatal("ordering markers raced: the callback returned before release")
	case <-time.After(300 * time.Millisecond):
	}

	close(release) // the callback returns; the join may complete now
	select {
	case <-cancelReturned:
	case <-time.After(20 * time.Second):
		t.Fatal("external cancel never returned after the delivery drained")
	}
	select {
	case <-returned:
	default:
		t.Fatal("cancel returned before the in-flight callback did")
	}
}

// TestListenCloseWaitsInFlightDelivery: the exposure rule is ordered BOTH
// ways — no records delivered after closed fires, and closed never races
// a record the session is handing out: an end landing while a delivery
// is mid-notify (its mint already committed) waits the bracket out before
// firing, so a consumer tearing down inside closed cannot beat the record
// to its own callback (codex P1 on #228).
func TestListenCloseWaitsInFlightDelivery(t *testing.T) {
	st := openChangeStore(t)
	closedCause := make(chan error, 1)

	inFlight := make(chan ChangeRecord, 1)
	release := make(chan struct{})
	replay, cancel := listenOn(t, st, "", "", func(r ChangeRecord) {
		inFlight <- r
		<-release // park mid-notify: an engine end must wait this out
	}, func(cause error) { closedCause <- cause })
	defer cancel()

	// The boundary call releases the drainer before any delivery.
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}
	insertNotes(t, st, 1)
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("no live delivery arrived to park the close against")
	}

	// Overflow ends the session while the delivery is parked mid-notify.
	if _, err := insertNotesChunkedErr(st, listenQueueBound+MaxChangesPageLimit); err != nil {
		t.Fatalf("bulk write: %v", err)
	}
	select {
	case cause := <-closedCause:
		t.Fatalf("closed fired with %v while a delivery was still mid-notify — the fire must wait the bracket out", cause)
	case <-time.After(300 * time.Millisecond):
	}

	close(release) // the delivery returns; the waiting fire may land
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenOverflow) {
			t.Fatalf("close cause = %v, want ErrListenOverflow", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the waiting close never fired after the delivery returned")
	}
}
