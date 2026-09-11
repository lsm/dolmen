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
	// The teardown landed: the replay reports the session's end once it
	// lands, and neither callback ever deadlocks.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, _, done, err := replay.Next(context.Background())
		if errors.Is(err, errListenEnded) {
			break
		}
		if err != nil {
			t.Fatalf("post-teardown Next: %v", err)
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replay never reported its end after the teardown")
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

// TestListenExternalCancelJoinsPendingFire: the firing carve-out covers
// exactly the closedFn callback's EXECUTION — never the interval where an
// engine end WAITS a blocked delivery before firing. An external cancel
// arriving in that interval takes the quiescence join: it returns only
// after the in-flight notify returned AND the fire completed on the
// counted pump (codex P1 on #228, thread r3984384330).
func TestListenExternalCancelJoinsPendingFire(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 1)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	inFlight := make(chan struct{}, 1)
	release := make(chan struct{})
	goEnd := make(chan struct{})
	closedFired := make(chan error, 1)
	sess := testSession(func(cause error) { closedFired <- cause })
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)
	sess.notify = func(ChangeRecord) {
		inFlight <- struct{}{}
		<-release // park mid-notify: the engine end parks behind it
	}
	sess.queue = []loggedChange{{seq: 1}}
	sess.replayDone = true
	sess.pumps.Add(2)
	go sess.drain()
	go func() { // the engine end, on a counted pump as the engine shapes it
		defer sess.pumps.Done()
		<-goEnd
		sess.end(ErrListenOverflow) // parks WAITING the bracket; firing stays down
	}()
	<-inFlight // the delivery is parked mid-notify
	close(goEnd)
	// The engine end must WIN the dead race: once it has flipped dead,
	// its cause is parked (atomically with the flip) and a cancel can no
	// longer suppress the fire with its own nil-cause end. Waiting on
	// dead here makes the cancel's arrival deterministic.
	deadline := time.Now().Add(10 * time.Second)
	for {
		sess.mu.Lock()
		died := sess.dead
		sess.mu.Unlock()
		if died {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine end never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancelReturned := make(chan struct{})
	go func() {
		sess.cancel() // external: joins the pending fire
		close(cancelReturned)
	}()
	select {
	case <-cancelReturned:
		t.Fatal("cancel returned while the fire was still pending — the carve-out covered a wait, not the callback")
	case cause := <-closedFired:
		t.Fatalf("the fire landed (%v) before the blocked delivery returned", cause)
	case <-time.After(300 * time.Millisecond):
	}

	close(release) // the delivery returns; the end's wait wakes and fires
	select {
	case cause := <-closedFired:
		if !errors.Is(cause, ErrListenOverflow) {
			t.Fatalf("pending fire cause = %v, want ErrListenOverflow", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pending fire never landed after the delivery returned")
	}
	select {
	case <-cancelReturned:
	case <-time.After(10 * time.Second):
		t.Fatal("the joined cancel never returned after the teardown drained")
	}
}

// TestListenDrainFiresPendingDrainCloseAtEmptyQueue: the queue-owned
// terminal — armed by the queue's owner (a lifetime end, a revocation)
// once its admitted prefix has queued — fires from the drainer at the
// EMPTY queue, after every queued record has delivered, never before.
func TestListenDrainFiresPendingDrainCloseAtEmptyQueue(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	delivered := make(chan int64, 2)
	closedCause := make(chan error, 1)
	sess := testSession(func(cause error) { closedCause <- cause })
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)
	sess.notify = func(r ChangeRecord) { delivered <- r.RowID }
	sess.queue = []loggedChange{
		{seq: 1, rec: ChangeRecord{RowID: 1}},
		{seq: 2, rec: ChangeRecord{RowID: 2}},
	}
	sess.replayDone = true
	sess.pendingDrainClose = ErrListenLifetimeEnded // the armer's stand-in; the armer itself lands with Δ3
	sess.pumps.Add(1)
	go sess.drain()

	// Both queued records deliver BEFORE the close: the prefix drains.
	for i := 0; i < 2; i++ {
		select {
		case id := <-delivered:
			if id != int64(1+i) {
				t.Fatalf("queued record delivered out of order: row %d, want %d", id, 1+i)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("queued record %d never delivered before the close", i)
		}
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenLifetimeEnded) {
			t.Fatalf("queue-owned close cause = %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("queue-owned terminal never fired at the empty queue")
	}
	sess.cancel() // idempotent teardown; also waits the drain out
}

// TestListenLifetimeEndDrainsPredecessorBatch is Δ3's pin (codex P1 on
// #220, thread r3983157270): an event committed on the predecessor
// lifetime, then a drop that takes the write connection before the fill's
// read — the fill's batch scans UNDER THE REGISTRATION LABELS first, so
// the committed record queues and DELIVERS before ErrListenLifetimeEnded
// closes the session. The check-first ordering lost exactly this record:
// a reconnect on the old feed's cursor can never recover it.
func TestListenLifetimeEndDrainsPredecessorBatch(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	insertNotes(t, st, 1) // the predecessor's committed event, seq 1

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	// The registration labels, as a table-feed registration fixes them —
	// captured BEFORE the drop ends the lifetime.
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("labels tx: %v", err)
	}
	feed, err := changeFeedOf(ctx, tx, "test", "notes")
	if err != nil {
		tx.Rollback()
		t.Fatalf("labels: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("labels commit: %v", err)
	}
	// The drop: the table's lifetime ends AFTER the commit.
	if err := st.DropTable(ctx, "test", "notes", Incarnation{}); err != nil {
		t.Fatalf("drop: %v", err)
	}

	delivered := make(chan int64, 1)
	closedCause := make(chan error, 1)
	sess := testSession(func(cause error) { closedCause <- cause })
	sess.s, sess.n = st, n
	sess.feed = feed
	sess.chain = newCursorChain(time.Now(), 0)

	// The fill's next batch — the exact racing read the finding describes —
	// must queue the predecessor record AND arm the queue-owned close.
	if _, err := sess.fillBatch(); err != nil {
		t.Fatalf("fillBatch: %v", err)
	}
	sess.mu.Lock()
	queued := len(sess.queue)
	armed := sess.pendingDrainClose
	sess.replayDone = true // the boundary call has completed: the drainer runs
	sess.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queue held %d records, want the predecessor's 1 committed event", queued)
	}
	if !errors.Is(armed, ErrListenLifetimeEnded) {
		t.Fatalf("queue-owned terminal = %v, want ErrListenLifetimeEnded", armed)
	}
	sess.notify = func(r ChangeRecord) { delivered <- r.RowID }
	sess.pumps.Add(1)
	go sess.drain()

	select {
	case id := <-delivered:
		if id != 1 {
			t.Fatalf("delivered row %d, want the predecessor's row 1", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the predecessor's committed event never delivered — lost to the lifetime end")
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenLifetimeEnded) {
			t.Fatalf("lifetime-end close cause = %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lifetime end never closed the session after the prefix drained")
	}
	sess.cancel() // idempotent teardown; also waits the drain out
}

// TestListenLifetimeEndDrainsFullPages: a predecessor backlog spanning
// MORE than one page must drain COMPLETELY before the lifetime close —
// a full page with ended does not arm; the fill keeps paging the
// registration-fixed feed until its short tail, and only that final
// batch's close arms (codex P1 on #230, thread r3984677145).
func TestListenLifetimeEndDrainsFullPages(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	total := 2*MaxChangesPageLimit + 5
	if _, err := insertNotesChunkedErr(st, total); err != nil {
		t.Fatalf("bulk backlog: %v", err)
	}

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	// The registration labels, captured BEFORE the drop ends the lifetime.
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("labels tx: %v", err)
	}
	feed, err := changeFeedOf(ctx, tx, "test", "notes")
	if err != nil {
		tx.Rollback()
		t.Fatalf("labels: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("labels commit: %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes", Incarnation{}); err != nil {
		t.Fatalf("drop: %v", err)
	}

	var mu sync.Mutex
	delivered := 0
	closedCause := make(chan error, 1)
	sess := testSession(func(cause error) { closedCause <- cause })
	sess.s, sess.n = st, n
	sess.feed = feed
	sess.chain = newCursorChain(time.Now(), 0)
	sess.notify = func(r ChangeRecord) {
		mu.Lock()
		delivered++
		mu.Unlock()
	}
	sess.replayDone = true
	sess.pumps.Add(2)
	sess.wake("notes", ChangeRange{}) // the drop raced the pump: take the work now
	go sess.pump()
	go sess.drain()

	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenLifetimeEnded) {
			t.Fatalf("close cause = %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the lifetime close never fired")
	}
	mu.Lock()
	got := delivered
	mu.Unlock()
	if got != total {
		t.Fatalf("delivered %d of %d predecessor records before the close — a full page armed early and stranded the tail", got, total)
	}
	sess.cancel() // idempotent teardown; also waits the pumps out
}

// TestListenEndedFeedParksAtTheBound: an ENDED feed drains its
// predecessor backlog under BACKPRESSURE, never around the queue bound —
// the backlog is not size-bounded (retention off, or any volume inside a
// time window), so queueing it whole could exhaust process memory, while
// the overflow close would teach a reconnect that cannot succeed against
// a dropped target (codex P1 on #230, thread r3984838673). With no
// drainer freeing capacity, the fill PARKS at bound + one page — the
// memory contract held, no overflow end fired — and cancel still
// returns: the parked fill exits on end's broadcast (teardown liveness).
func TestListenEndedFeedParksAtTheBound(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	total := listenQueueBound + 2*MaxChangesPageLimit
	if _, err := insertNotesChunkedErr(st, total); err != nil {
		t.Fatalf("bulk backlog: %v", err)
	}

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("labels tx: %v", err)
	}
	feed, err := changeFeedOf(ctx, tx, "test", "notes")
	if err != nil {
		tx.Rollback()
		t.Fatalf("labels: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("labels commit: %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes", Incarnation{}); err != nil {
		t.Fatalf("drop: %v", err)
	}

	closedCause := make(chan error, 1)
	sess := testSession(func(cause error) { closedCause <- cause })
	sess.s, sess.n = st, n
	sess.feed = feed
	sess.chain = newCursorChain(time.Now(), 0)
	sess.pumps.Add(1)
	sess.wake("notes", ChangeRange{})
	go sess.pump()

	// The fill reaches the bound and parks: no drainer is freeing
	// capacity, so the queue must stabilize at bound + one page — the
	// whole backlog (2 pages past the bound) must NOT materialize.
	reach := time.Now().Add(60 * time.Second)
	for {
		sess.mu.Lock()
		queued := len(sess.queue)
		dead := sess.dead
		sess.mu.Unlock()
		if dead {
			t.Fatalf("session died mid-drain at %d queued — the overflow close fired on an ended feed", queued)
		}
		if queued > listenQueueBound {
			break // the batch that crossed the bound is in; the fill parks behind it
		}
		if time.Now().After(reach) {
			t.Fatalf("fill never reached the bound (queued=%d)", queued)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // any unbounded paging would keep growing here
	sess.mu.Lock()
	queued := len(sess.queue)
	dead := sess.dead
	sess.mu.Unlock()
	if dead {
		t.Fatal("session died while parked at the bound")
	}
	if queued > listenQueueBound+MaxChangesPageLimit {
		t.Fatalf("queue held %d records, want the fill parked at ≤ bound+page (%d) — the ended feed bypassed the bound", queued, listenQueueBound+MaxChangesPageLimit)
	}
	select {
	case cause := <-closedCause:
		t.Fatalf("closed fired with %v while the fill was parked — the overflow close must not take an ended feed", cause)
	default:
	}

	// Teardown liveness: the parked fill exits on end's broadcast.
	done := make(chan struct{})
	go func() {
		sess.cancel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("cancel never returned — the backpressure wait outlived the session")
	}
}

// TestListenEndedFeedDrainsUnderBackpressure: the mechanism's full
// promise end to end — an ended feed whose backlog crosses the queue
// bound delivers EVERY predecessor record (the fill parks at the bound,
// the drainer frees capacity, the fill resumes) before
// ErrListenLifetimeEnded fires at the empty queue. The queue never
// exceeds bound + one page while it does.
func TestListenEndedFeedDrainsUnderBackpressure(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	total := listenQueueBound + MaxChangesPageLimit + 5
	if _, err := insertNotesChunkedErr(st, total); err != nil {
		t.Fatalf("bulk backlog: %v", err)
	}

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("labels tx: %v", err)
	}
	feed, err := changeFeedOf(ctx, tx, "test", "notes")
	if err != nil {
		tx.Rollback()
		t.Fatalf("labels: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("labels commit: %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes", Incarnation{}); err != nil {
		t.Fatalf("drop: %v", err)
	}

	var mu sync.Mutex
	peak, delivered := 0, 0
	closedCause := make(chan error, 1)
	sess := testSession(func(cause error) { closedCause <- cause })
	sess.s, sess.n = st, n
	sess.feed = feed
	sess.chain = newCursorChain(time.Now(), 0)
	sess.notify = func(r ChangeRecord) {
		sess.mu.Lock()
		q := len(sess.queue)
		sess.mu.Unlock()
		mu.Lock()
		delivered++
		if q > peak {
			peak = q
		}
		mu.Unlock()
	}
	sess.replayDone = true
	sess.pumps.Add(2)
	sess.wake("notes", ChangeRange{})
	go sess.pump()
	go sess.drain()

	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenLifetimeEnded) {
			t.Fatalf("close cause = %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("the lifetime close never fired under backpressure")
	}
	mu.Lock()
	got, ceiling := delivered, peak
	mu.Unlock()
	if got != total {
		t.Fatalf("delivered %d of %d predecessor records — the backpressure lost part of the backlog", got, total)
	}
	if ceiling > listenQueueBound+MaxChangesPageLimit {
		t.Fatalf("queue peaked at %d, want ≤ bound+page (%d) — the bound was bypassed", ceiling, listenQueueBound+MaxChangesPageLimit)
	}
	sess.cancel() // idempotent teardown; also waits the pumps out
}
