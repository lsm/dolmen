package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func TestListenDeliversLiveAfterReplayDrains(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	got := make(chan ChangeRecord, 4)
	replay, cancel := listenOn(t, st, "", CursorBegin, func(r ChangeRecord) { got <- r }, nil)
	defer cancel()

	insertNotes(t, st, 2)

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

func TestListenDrainWaitsForBoundaryCall(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	got := make(chan ChangeRecord, 4)
	replay, cancel := listenOn(t, st, "", CursorBegin, func(r ChangeRecord) { got <- r }, nil)
	defer cancel()

	insertNotes(t, st, 2)

	noEarly := func(stage string) {
		select {
		case r := <-got:
			t.Fatalf("live record (row %d) delivered at %s — before the boundary call completed", r.RowID, stage)
		case <-time.After(200 * time.Millisecond):
		}
	}
	noEarly("rest")

	if _, _, done, err := replay.Next(context.Background()); err != nil || done {
		t.Fatalf("first Next = err %v, done %v, want nil error, done=false (the final records)", err, done)
	}
	noEarly("the final-records page")

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
	sess.queue = []loggedChange{{seq: 999}}
	sess.replayDone = true
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
	sess.cancel()
}

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

func TestListenNotifyDefersCancel(t *testing.T) {
	st := openChangeStore(t)
	toredown := make(chan struct{})
	var unsubscribe sync.Once

	var cancel func()
	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {

		unsubscribe.Do(func() { go cancel(); close(toredown) })
	}, nil)
	defer cancel()

	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}
	insertNotes(t, st, 2)

	select {
	case <-toredown:
	case <-time.After(20 * time.Second):
		t.Fatal("no delivery arrived to unsubscribe on")
	}

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

func TestListenExternalCancelWaitsInFlightDelivery(t *testing.T) {
	st := openChangeStore(t)

	inFlight := make(chan struct{}, 1)
	returned := make(chan struct{})
	release := make(chan struct{})
	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {
		inFlight <- struct{}{}
		<-release
		close(returned)
	}, nil)

	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}
	insertNotes(t, st, 1)
	<-inFlight

	cancelReturned := make(chan struct{})
	go func() {
		cancel()
		close(cancelReturned)
	}()
	select {
	case <-cancelReturned:
		t.Fatal("external cancel returned while the in-flight notify was still parked — the quiescence guarantee torn")
	case <-returned:
		t.Fatal("ordering markers raced: the callback returned before release")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
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

func TestListenCloseWaitsInFlightDelivery(t *testing.T) {
	st := openChangeStore(t)
	closedCause := make(chan error, 1)

	inFlight := make(chan ChangeRecord, 1)
	release := make(chan struct{})
	replay, cancel := listenOn(t, st, "", "", func(r ChangeRecord) {
		inFlight <- r
		<-release
	}, func(cause error) { closedCause <- cause })
	defer cancel()

	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}
	insertNotes(t, st, 1)
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("no live delivery arrived to park the close against")
	}

	if _, err := insertNotesChunkedErr(st, ListenQueueBound+MaxChangesPageLimit); err != nil {
		t.Fatalf("bulk write: %v", err)
	}
	select {
	case cause := <-closedCause:
		t.Fatalf("closed fired with %v while a delivery was still mid-notify — the fire must wait the bracket out", cause)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenOverflow) {
			t.Fatalf("close cause = %v, want ErrListenOverflow", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the waiting close never fired after the delivery returned")
	}
}

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
		<-release
	}
	sess.queue = []loggedChange{{seq: 1}}
	sess.replayDone = true
	sess.pumps.Add(2)
	go sess.drain()
	go func() {
		defer sess.pumps.Done()
		<-goEnd
		sess.end(ErrListenOverflow)
	}()
	<-inFlight
	close(goEnd)

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
		sess.cancel()
		close(cancelReturned)
	}()
	select {
	case <-cancelReturned:
		t.Fatal("cancel returned while the fire was still pending — the carve-out covered a wait, not the callback")
	case cause := <-closedFired:
		t.Fatalf("the fire landed (%v) before the blocked delivery returned", cause)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
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
	sess.pendingDrainClose = ErrListenLifetimeEnded
	sess.pumps.Add(1)
	go sess.drain()

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
	sess.cancel()
}

func TestListenLifetimeEndDrainsPredecessorBatch(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	insertNotes(t, st, 1)

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

	delivered := make(chan int64, 1)
	closedCause := make(chan error, 1)
	sess := testSession(func(cause error) { closedCause <- cause })
	sess.s, sess.n = st, n
	sess.feed = feed
	sess.chain = newCursorChain(time.Now(), 0)

	if _, err := sess.fillBatch(); err != nil {
		t.Fatalf("fillBatch: %v", err)
	}
	sess.mu.Lock()
	queued := len(sess.queue)
	armed := sess.pendingDrainClose
	sess.replayDone = true
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
	sess.cancel()
}

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
	sess.wake("notes", ChangeRange{})
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
	sess.cancel()
}

func TestListenEndedFeedParksAtTheBound(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	total := ListenQueueBound + 2*MaxChangesPageLimit
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

	reach := time.Now().Add(60 * time.Second)
	for {
		sess.mu.Lock()
		queued := len(sess.queue)
		dead := sess.dead
		sess.mu.Unlock()
		if dead {
			t.Fatalf("session died mid-drain at %d queued — the overflow close fired on an ended feed", queued)
		}
		if queued > ListenQueueBound {
			break
		}
		if time.Now().After(reach) {
			t.Fatalf("fill never reached the bound (queued=%d)", queued)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	sess.mu.Lock()
	queued := len(sess.queue)
	dead := sess.dead
	sess.mu.Unlock()
	if dead {
		t.Fatal("session died while parked at the bound")
	}
	if queued > ListenQueueBound+MaxChangesPageLimit {
		t.Fatalf("queue held %d records, want the fill parked at ≤ bound+page (%d) — the ended feed bypassed the bound", queued, ListenQueueBound+MaxChangesPageLimit)
	}
	select {
	case cause := <-closedCause:
		t.Fatalf("closed fired with %v while the fill was parked — the overflow close must not take an ended feed", cause)
	default:
	}

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

func TestListenEndedFeedDrainsUnderBackpressure(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	total := ListenQueueBound + MaxChangesPageLimit + 5
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
	if ceiling > ListenQueueBound+MaxChangesPageLimit {
		t.Fatalf("queue peaked at %d, want ≤ bound+page (%d) — the bound was bypassed", ceiling, ListenQueueBound+MaxChangesPageLimit)
	}
	sess.cancel()
}
