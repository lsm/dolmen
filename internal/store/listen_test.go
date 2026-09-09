package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Slice 6b: Listen — the atomic register-and-replay session. The fixtures
// drive the real write paths (like the 5c suite) and observe both halves
// exactly as a caller would: replay through ChangeReplay.Next, live through
// the notify callback, engine-initiated ends through closed.

// liveLog is the test's notify collector: every delivered record, plus a
// cond so a test can wait for delivery counts without polling.
type liveLog struct {
	mu   sync.Mutex
	cond *sync.Cond
	recs []ChangeRecord
}

func newLiveLog() *liveLog {
	l := &liveLog{}
	l.cond = sync.NewCond(&l.mu)
	return l
}

func (l *liveLog) notify(rec ChangeRecord) {
	l.mu.Lock()
	l.recs = append(l.recs, rec)
	l.cond.Broadcast()
	l.mu.Unlock()
}

// waitN blocks until n records have been delivered, failing the test on the
// deadline instead of hanging a stuck session forever.
func (l *liveLog) waitN(t *testing.T, n int) []ChangeRecord {
	t.Helper()
	delivered := make(chan struct{})
	go func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for len(l.recs) < n {
			l.cond.Wait()
		}
		close(delivered)
	}()
	select {
	case <-delivered:
	case <-time.After(20 * time.Second):
		l.mu.Lock()
		count := len(l.recs)
		l.mu.Unlock()
		t.Fatalf("delivered %d live records, waiting for %d", count, n)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recs
}

// count snapshots the delivered record count.
func (l *liveLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recs)
}

// rowSet projects records as a counted set of row ids — the exactly-once
// assertions compare counted sets, not slices.
func rowSet(records []ChangeRecord) map[int64]int {
	out := map[int64]int{}
	for _, r := range records {
		out[r.RowID]++
	}
	return out
}

// drainReplay pages the replay half to done, collecting records.
func drainReplay(t *testing.T, replay *ChangeReplay) []ChangeRecord {
	t.Helper()
	var out []ChangeRecord
	for {
		records, _, done, err := replay.Next(context.Background())
		if err != nil {
			t.Fatalf("replay Next: %v", err)
		}
		out = append(out, records...)
		if done {
			return out
		}
	}
}

// listenOn is Listen with the 6b-era defaults a caller under auth:off uses:
// no nsGen guard, no per-event authorization.
func listenOn(t *testing.T, st *Store, table string, from Cursor, notify func(ChangeRecord), closed func(error)) (*ChangeReplay, func()) {
	t.Helper()
	replay, cancel, err := st.Listen(context.Background(), "test", table, from, [16]byte{}, nil, notify, closed)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return replay, cancel
}

// insertNotesErr is insertNotes for goroutines the test cannot Fatalf on:
// the error crosses back to the test goroutine instead.
func insertNotesErr(st *Store, n int) (InsertResult, error) {
	recs := make([]map[string]any, n)
	for i := range recs {
		recs[i] = map[string]any{"title": string(rune('a' + i%26)), "score": i + 1}
	}
	return st.Insert(context.Background(), "test", "notes", recs, WriteOpts{}, Embedder{}, nil, Incarnation{})
}

// TestListenReplayThenLive: the core shape — a session holding a cursor
// replays exactly the backlog after it, then stays open and delivers live
// commits through notify, in order, with a fresh cursor per record (§6.2,
// §9.3).
func TestListenReplayThenLive(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 3)

	live := newLiveLog()
	replay, cancel := listenOn(t, st, "", CursorBegin, live.notify, nil)
	defer cancel()

	replayed := drainReplay(t, replay)
	if got := rowIDsOf(replayed); len(got) != 3 || got[0] != backlog.Ids[0] || got[2] != backlog.Ids[2] {
		t.Fatalf("replay delivered rows %v, want the 3-record backlog %v", got, backlog.Ids)
	}
	for _, rec := range replayed {
		if rec.Cursor == "" {
			t.Fatalf("replay record for row %d carries no cursor", rec.RowID)
		}
	}

	fresh := insertNotes(t, st, 2)
	delivered := live.waitN(t, 2)
	if got := rowIDsOf(delivered); got[0] != fresh.Ids[0] || got[1] != fresh.Ids[1] {
		t.Fatalf("live delivered rows %v, want %v", got, fresh.Ids)
	}
	for _, rec := range delivered {
		if rec.Cursor == "" {
			t.Fatalf("live record for row %d carries no cursor", rec.RowID)
		}
	}
}

// TestListenExactlyOnceAcrossBoundary: the slice's core conformance claim —
// a write landing DURING replay is neither duplicated nor skipped (§6.2's
// replay-then-live concatenation is exactly cursor order). Concurrent
// writers commit across the whole replay window; every record must arrive
// exactly once — replay covering the pre-registration backlog only, live the
// rest, in strictly increasing commit order.
func TestListenExactlyOnceAcrossBoundary(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 50)

	live := newLiveLog()
	replay, cancel := listenOn(t, st, "", CursorBegin, live.notify, nil)
	defer cancel()

	// Four writers commit across the replay window.
	const writers, each = 4, 50
	var wg sync.WaitGroup
	writeErr := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := insertNotesErr(st, each); err != nil {
				writeErr <- err
			}
		}()
	}
	replayed := drainReplay(t, replay)
	wg.Wait()
	select {
	case err := <-writeErr:
		t.Fatalf("concurrent write: %v", err)
	default:
	}

	// Replay covers exactly the backlog, each record once: the boundary was
	// fixed at registration, before any writer committed.
	replaySet := rowSet(replayed)
	if len(replaySet) != len(backlog.Ids) {
		t.Fatalf("replay delivered %d distinct rows, want exactly the %d-record backlog", len(replaySet), len(backlog.Ids))
	}
	for _, id := range backlog.Ids {
		if got := replaySet[id]; got != 1 {
			t.Fatalf("backlog row %d appeared %d times in replay, want 1", id, got)
		}
	}

	// Live covers exactly the writers' records, each once, in commit order
	// (ids and seqs are assigned under the same write lock, so row order is
	// cursor order), and none of them replayed.
	delivered := live.waitN(t, writers*each)
	liveSet := rowSet(delivered)
	if len(liveSet) != writers*each {
		t.Fatalf("live delivered %d distinct rows, want %d", len(liveSet), writers*each)
	}
	var last int64
	for _, rec := range delivered {
		if liveSet[rec.RowID] != 1 {
			t.Fatalf("live row %d delivered %d times, want 1", rec.RowID, liveSet[rec.RowID])
		}
		if _, replayed := replaySet[rec.RowID]; replayed {
			t.Fatalf("live row %d was also replayed — the boundary duplicated a record", rec.RowID)
		}
		if rec.RowID <= last {
			t.Fatalf("live delivery out of cursor order: row %d after %d", rec.RowID, last)
		}
		last = rec.RowID
	}
}

// TestListenPagedReplayDoneContract pins ChangeReplay's done protocol
// exactly: pages of MaxChangesPageLimit, the page carrying the final records
// reports done=false, the following call reports done=true with no records —
// the registration boundary (§6.2).
func TestListenPagedReplayDoneContract(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2*MaxChangesPageLimit+500)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()
	ctx := context.Background()

	for page, wantLen := range []int{MaxChangesPageLimit, MaxChangesPageLimit, 500} {
		records, next, done, err := replay.Next(ctx)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(records) != wantLen {
			t.Fatalf("page %d = %d records, want %d", page, len(records), wantLen)
		}
		if done {
			t.Fatalf("page %d reported done=true, want false — the final page still reports false", page)
		}
		if next == "" {
			t.Fatalf("page %d carried no next cursor", page)
		}
	}
	records, _, done, err := replay.Next(ctx)
	if err != nil {
		t.Fatalf("boundary call: %v", err)
	}
	if len(records) != 0 || !done {
		t.Fatalf("boundary call = %d records, done=%v, want 0 records, done=true", len(records), done)
	}
}

// TestListenBareStartIsLiveOnly: the zero cursor fixes the boundary at the
// current head — the replay is empty (first Next reports done immediately)
// and only subsequent commits arrive (§9.3's wake-up semantics).
func TestListenBareStartIsLiveOnly(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 3)

	live := newLiveLog()
	replay, cancel := listenOn(t, st, "", "", live.notify, nil)
	defer cancel()

	records, _, done, err := replay.Next(context.Background())
	if err != nil {
		t.Fatalf("bare-start Next: %v", err)
	}
	if len(records) != 0 || !done {
		t.Fatalf("bare-start replay = %d records, done=%v, want 0, true", len(records), done)
	}

	fresh := insertNotes(t, st, 1)
	delivered := live.waitN(t, 1)
	if delivered[0].RowID != fresh.Ids[0] {
		t.Fatalf("bare-start live delivered row %d, want %d", delivered[0].RowID, fresh.Ids[0])
	}
}

// TestListenOverflowTeachingClose: a subscriber that stops draining while
// its own visible commits keep landing overflows the bounded buffer, and the
// engine ends the session with the teaching reconnect cause — not a hang,
// not a silent drop (§6.2: the log is durable; the buffer never is).
func TestListenOverflowTeachingClose(t *testing.T) {
	st := openChangeStore(t)
	block := make(chan struct{}) // the slowest possible subscriber
	live := newLiveLog()
	closedCause := make(chan error, 1)

	replay, cancel := listenOn(t, st, "", "", func(rec ChangeRecord) {
		<-block // the subscriber reads nothing until the test releases it
		live.notify(rec)
	}, func(cause error) { closedCause <- cause })
	// Registered in this order so LIFO teardown releases the stuck drainer
	// (close(block)) BEFORE cancel waits for it to exit — a failure path that
	// skips the explicit close must not hang the test binary.
	defer cancel()
	defer close(block)
	drainReplay(t, replay)

	// One bulk commit past the bound: fill batches stack the queue while the
	// drainer is stuck inside its first notify.
	if _, err := insertNotesErr(st, listenQueueBound+MaxChangesPageLimit); err != nil {
		t.Fatalf("bulk write: %v", err)
	}

	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenOverflow) {
			t.Fatalf("overflow close cause = %v, want ErrListenOverflow", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no overflow close: the bounded buffer never tripped")
	}
}

// TestListenCancelReleasesListener: cancel ends the session — later commits
// deliver nothing, closed never fires, the replay reports done, and cancel
// is idempotent and quiescent (notify.go's teardown rule).
func TestListenCancelReleasesListener(t *testing.T) {
	st := openChangeStore(t)
	live := newLiveLog()
	closedFired := make(chan error, 1)

	replay, cancel := listenOn(t, st, "", CursorBegin, live.notify, func(cause error) { closedFired <- cause })
	drainReplay(t, replay)

	cancel()
	cancel() // idempotent

	insertNotes(t, st, 5)
	time.Sleep(100 * time.Millisecond)
	if n := live.count(); n != 0 {
		t.Fatalf("%d records delivered after cancel, want 0", n)
	}
	select {
	case cause := <-closedFired:
		t.Fatalf("closed fired on caller cancel with %v", cause)
	case <-time.After(100 * time.Millisecond):
	}

	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("Next after cancel = err %v, done %v, want nil error, done=true", err, done)
	}
}

// TestListenLiveAuthzAdmission: per-event authorization runs BEFORE queue
// admission and is LIVE (§6.2): a scope filters by the record's Owner label,
// an Empty scope sees nothing, and ok=false teaching-closes the stream —
// during replay exactly as during live delivery.
func TestListenLiveAuthzAdmission(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	insertAs := func(n int, owner string) InsertResult {
		recs := make([]map[string]any, n)
		for i := range recs {
			recs[i] = map[string]any{"title": "t", "score": i + 1}
		}
		res, err := st.Insert(ctx, "test", "notes", recs, WriteOpts{Owner: owner}, Embedder{}, nil, Incarnation{})
		if err != nil {
			t.Fatalf("insert as %s: %v", owner, err)
		}
		return res
	}
	aliceBacklog := insertAs(2, "alice") // the replayed backlog, owner-stamped
	insertAs(2, "bob")                   // invisible to alice

	// A scoped viewer sees exactly their own rows, in both halves.
	live := newLiveLog()
	scoped := func(string) (*RowScope, Incarnation, bool) {
		return &RowScope{Owner: "alice"}, Incarnation{}, true
	}
	replay, cancel, err := st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, scoped, live.notify, nil)
	if err != nil {
		t.Fatalf("scoped listen: %v", err)
	}
	defer cancel()
	replaySet := rowSet(drainReplay(t, replay))
	for _, id := range aliceBacklog.Ids {
		if replaySet[id] != 1 {
			t.Fatalf("scoped replay: alice's row %d appeared %d times, want 1", id, replaySet[id])
		}
	}
	if len(replaySet) != len(aliceBacklog.Ids) {
		t.Fatalf("scoped replay delivered %d rows, want alice's %d only", len(replaySet), len(aliceBacklog.Ids))
	}
	freshAlice := insertAs(1, "alice")
	insertAs(1, "bob")
	delivered := live.waitN(t, 1)
	if delivered[0].RowID != freshAlice.Ids[0] {
		t.Fatalf("scoped live delivered row %d, want alice's %d only", delivered[0].RowID, freshAlice.Ids[0])
	}
	cancel()

	// An Empty scope sees nothing at all.
	empty := newLiveLog()
	emptyScope := func(string) (*RowScope, Incarnation, bool) {
		return &RowScope{Empty: true}, Incarnation{}, true
	}
	replay, cancel, err = st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, emptyScope, empty.notify, nil)
	if err != nil {
		t.Fatalf("empty-scope listen: %v", err)
	}
	defer cancel()
	if got := rowIDsOf(drainReplay(t, replay)); len(got) != 0 {
		t.Fatalf("empty-scope replay delivered %v, want nothing", got)
	}

	// ok=false mid-replay is a teaching close, never a silent skip: closed
	// fires with the revocation cause and the records after the revoked one
	// are never exposed.
	seen := 0
	revokeAfterFirst := func(string) (*RowScope, Incarnation, bool) {
		seen++
		if seen > 1 {
			return nil, Incarnation{}, false
		}
		return nil, Incarnation{}, true
	}
	closedCause := make(chan error, 1)
	replay, cancel, err = st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, revokeAfterFirst, newLiveLog().notify,
		func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("revocation listen: %v", err)
	}
	defer cancel()
	if got := rowIDsOf(drainReplay(t, replay)); len(got) != 1 {
		t.Fatalf("revoked replay delivered %v, want the 1 record admitted before revocation", got)
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenRevoked) {
			t.Fatalf("revocation cause = %v, want ErrListenRevoked", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revocation never closed the session")
	}
}

// TestListenTableFeedLifetime: a table feed follows ONE table lifetime — it
// sees only its table's records, and a drop mid-stream ends the session with
// the teaching lifetime cause; a same-named successor is a different feed
// and never continues the stream.
func TestListenTableFeedLifetime(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "tasks", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create tasks: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "tasks", []map[string]any{{"title": "t", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert tasks: %v", err)
	}
	notes := insertNotes(t, st, 1)

	live := newLiveLog()
	closedCause := make(chan error, 1)
	replay, cancel, err := st.Listen(ctx, "test", "notes", CursorBegin, [16]byte{}, nil, live.notify,
		func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("table listen: %v", err)
	}
	defer cancel()
	if got := rowIDsOf(drainReplay(t, replay)); len(got) != 1 || got[0] != notes.Ids[0] {
		t.Fatalf("table feed replayed %v, want only the notes row %v", got, notes.Ids)
	}

	// The drop ends the feed: the table's lifetime is the feed's identity.
	if err := st.DropTable(ctx, "test", "notes", Incarnation{}); err != nil {
		t.Fatalf("drop notes: %v", err)
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenLifetimeEnded) {
			t.Fatalf("drop close cause = %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dropped-table feed never closed")
	}

	// A recreated successor is a different feed: its commits never arrive on
	// the ended session's stream.
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("recreate notes: %v", err)
	}
	insertNotes(t, st, 1)
	time.Sleep(100 * time.Millisecond)
	if n := live.count(); n != 0 {
		t.Fatalf("successor's commit arrived on the predecessor's stream: %d records", n)
	}
}

// TestListenNamespaceDropEndsSession: the session is bound to the namespace
// instance it registered on — a namespace drop ends it with the teaching
// lifetime cause, and a recreated successor's history never crosses the
// lifetime boundary (§9.3's cursor-lifetime rule, applied to streams).
func TestListenNamespaceDropEndsSession(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	insertNotes(t, st, 1)

	live := newLiveLog()
	closedCause := make(chan error, 1)
	replay, cancel, err := st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, nil, live.notify,
		func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	drainReplay(t, replay)

	if err := st.DropNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("drop namespace: %v", err)
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenLifetimeEnded) {
			t.Fatalf("drop close cause = %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dropped namespace never closed its session")
	}
	cancel()

	// A recreated successor is a different namespace: a fresh session on it
	// starts empty and never sees the predecessor's history.
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("recreate namespace: %v", err)
	}
	fresh := newLiveLog()
	replay2, cancel2, err := st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, nil, fresh.notify, nil)
	if err != nil {
		t.Fatalf("listen on successor: %v", err)
	}
	defer cancel2()
	if got := rowIDsOf(drainReplay(t, replay2)); len(got) != 0 {
		t.Fatalf("successor replayed %v — the predecessor's history must not cross the lifetime", got)
	}
}

// TestListenRegistrationErrors: the cursor family and the feed targets fail
// at REGISTRATION — inside the atomic operation — never half-way into a
// session (§6.2): unknown tokens, cross-feed reuse, a missing table, a
// missing namespace.
func TestListenRegistrationErrors(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	insertNotes(t, st, 1)
	noop := func(ChangeRecord) {}

	if _, _, err := st.Listen(ctx, "test", "", "never-minted", [16]byte{}, nil, noop, nil); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("unknown cursor = %v, want ErrCursorExpired", err)
	}
	_, tableCursor, err := st.ChangesSince(ctx, "test", "notes", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("changes_since on table feed: %v", err)
	}
	if _, _, err := st.Listen(ctx, "test", "", tableCursor, [16]byte{}, nil, noop, nil); !errors.Is(err, ErrCursorCrossFeed) {
		t.Fatalf("cross-feed cursor = %v, want ErrCursorCrossFeed", err)
	}
	if _, _, err := st.Listen(ctx, "test", "missing", "", [16]byte{}, nil, noop, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing table = %v, want ErrNotFound", err)
	}
	if _, _, err := st.Listen(ctx, "missing", "", "", [16]byte{}, nil, noop, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing namespace = %v, want ErrNotFound", err)
	}
	if _, _, err := st.Listen(ctx, "test", "", "", [16]byte{}, nil, nil, nil); err == nil {
		t.Fatal("nil notify = nil error, want rejection")
	}
}
