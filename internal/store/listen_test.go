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
// the error crosses back to the test goroutine instead. Large counts are
// chunked to the per-call record cap — the boundary math only needs the
// commits to land, and every chunk mints contiguous seqs.
func insertNotesErr(st *Store, n int) (InsertResult, error) {
	ctx := context.Background()
	var out InsertResult
	for n > 0 {
		size := n
		if size > MaxRecordsPerInsert {
			size = MaxRecordsPerInsert
		}
		recs := make([]map[string]any, size)
		for i := range recs {
			recs[i] = map[string]any{"title": string(rune('a' + i%26)), "score": i + 1}
		}
		res, err := st.Insert(ctx, "test", "notes", recs, WriteOpts{}, Embedder{}, nil, Incarnation{})
		if err != nil {
			return out, err
		}
		out.Ids = append(out.Ids, res.Ids...)
		out.Changes.Count += res.Changes.Count
		n -= size
	}
	return out, nil
}

// insertNotesChunked is insertNotesErr for the test goroutine (fail-fast).
func insertNotesChunked(t *testing.T, st *Store, n int) []int64 {
	t.Helper()
	res, err := insertNotesErr(st, n)
	if err != nil {
		t.Fatalf("insert %d notes: %v", n, err)
	}
	return res.Ids
}

// seedOwnerChanges writes change records directly, each carrying an owner
// label — the out-of-band fixture for scoped admission, which no public
// write path can mint yet (every path passes nil owners until slice 9c
// stamps them). Rows carry the namespace's and table's current lifetime
// labels so the feed filters match them, and mint now.
func seedOwnerChanges(t *testing.T, st *Store, nsName, table string, owners []string) {
	t.Helper()
	ctx := context.Background()
	n, err := st.ns(nsName)
	if err != nil {
		t.Fatalf("open %s: %v", nsName, err)
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	nsGen, err := readNSGen(ctx, tx)
	if err != nil {
		t.Fatalf("read nsgen: %v", err)
	}
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		t.Fatalf("read drop gen: %v", err)
	}
	for i, owner := range owners {
		var labeled any
		if owner != "" {
			labeled = owner
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO _dolmen_changes(table_name, row_id, kind, owner, nsgen, drop_gen) VALUES(?,?,?,?,?,?)`,
			table, int64(i+1), string(ChangeInsert), labeled, nsGen[:], gen); err != nil {
			t.Fatalf("seed change row %d: %v", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
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
	insertNotesChunked(t, st, 2*MaxChangesPageLimit+500)

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
// not a silent drop (§6.2: the log is durable; the buffer never is). The
// close fires only once the in-flight delivery returns (the exposure rule:
// closed never precedes a record), so the fixture releases the stuck
// subscriber AFTER the overflow has parked the close — a live slow client
// unblocks its read eventually, and that is exactly when the teaching
// arrives.
func TestListenOverflowTeachingClose(t *testing.T) {
	st := openChangeStore(t)
	block := make(chan struct{}) // the slowest possible subscriber
	live := newLiveLog()
	closedCause := make(chan error, 1)

	replay, cancel := listenOn(t, st, "", "", func(rec ChangeRecord) {
		<-block // the subscriber reads nothing until the test releases it
		live.notify(rec)
	}, func(cause error) { closedCause <- cause })
	defer cancel()
	drainReplay(t, replay)

	// One bulk commit past the bound: fill batches stack the queue while the
	// drainer is stuck inside its first notify.
	if _, err := insertNotesErr(st, listenQueueBound+MaxChangesPageLimit); err != nil {
		t.Fatalf("bulk write: %v", err)
	}
	// Give the filler time to stack the queue past the bound and park the
	// overflow close behind the stuck delivery, then release the subscriber:
	// the delivery returns, the drainer stops on dead, and the parked close
	// fires from its quiescent exit.
	time.Sleep(1 * time.Second)
	close(block)

	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenOverflow) {
			t.Fatalf("overflow close cause = %v, want ErrListenOverflow", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no overflow close: the bounded buffer never tripped")
	}
	if n := live.count(); n > listenQueueBound+MaxChangesPageLimit {
		t.Fatalf("%d records delivered through an overflowed buffer", n)
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
// during replay exactly as during live delivery. Owner-labeled records are
// seeded out-of-band (seedOwnerChanges): no public write path stamps the
// change record's owner until slice 9c, and the live half's filter — the
// same admit — is pinned against real writes, whose records are unlabeled
// and therefore invisible to any scoped viewer.
func TestListenLiveAuthzAdmission(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	seedOwnerChanges(t, st, "test", "notes", []string{"alice", "alice", "bob"})

	// A scoped viewer sees exactly their own rows on the replay half.
	scoped := func(string) (*RowScope, Incarnation, bool) {
		return &RowScope{Owner: "alice"}, Incarnation{}, true
	}
	replay, cancel, err := st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, scoped, newLiveLog().notify, nil)
	if err != nil {
		t.Fatalf("scoped listen: %v", err)
	}
	if got := rowIDsOf(drainReplay(t, replay)); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("scoped replay delivered %v, want alice's rows [1 2] only", got)
	}
	cancel()

	// The live half through the same gate: real writes mint unlabeled
	// records, which a scoped viewer must never see.
	scopedLive := newLiveLog()
	replay, cancel, err = st.Listen(ctx, "test", "", "", [16]byte{}, scoped, scopedLive.notify, nil)
	if err != nil {
		t.Fatalf("scoped live listen: %v", err)
	}
	defer cancel()
	drainReplay(t, replay)
	insertNotes(t, st, 2)
	time.Sleep(100 * time.Millisecond)
	if n := scopedLive.count(); n != 0 {
		t.Fatalf("scoped live delivered %d unlabeled records, want 0 — a scope never admits an unlabeled row", n)
	}

	// An Empty scope sees nothing at all, replay and live alike.
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

	// ok=false mid-replay is a teaching close that precedes every exposed
	// record (§6.2: a consumer may tear down in its closed callback): the
	// page carrying the revoked record is omitted entirely — closed fires
	// with the cause, Next reports done, and the omitted records re-deliver
	// on the caller's reconnect from its last cursor.
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
	if got := rowIDsOf(drainReplay(t, replay)); len(got) != 0 {
		t.Fatalf("revoked replay delivered %v, want nothing exposed after closed", got)
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

// TestListenLiveRevocationDeliversPrefix: a revocation found mid-LIVE-batch
// delivers the batch's admitted prefix before the teaching close — the
// positions are consumed, and a revocation takes effect at the next event,
// not retroactively — with closed firing only after the last queued record
// (§6.2's exposure rule).
func TestListenLiveRevocationDeliversPrefix(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	admitted := 0
	revokeAfterTwo := func(string) (*RowScope, Incarnation, bool) {
		admitted++
		if admitted > 2 {
			return nil, Incarnation{}, false
		}
		return &RowScope{Owner: "alice"}, Incarnation{}, true
	}
	live := newLiveLog()
	closedCause := make(chan error, 1)
	replay, cancel, err := st.Listen(ctx, "test", "", "", [16]byte{}, revokeAfterTwo, live.notify,
		func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()
	drainReplay(t, replay)

	// Three owner-labeled records (seeded out-of-band — no public write
	// path stamps change-record owners until 9c), then a real unlabeled
	// write: the seed does not ride the write paths, so the commit is what
	// wakes the filler. The batch admits the two labeled records, revokes at
	// the third; the unlabeled record is never reached.
	seedOwnerChanges(t, st, "test", "notes", []string{"alice", "alice", "alice"})
	insertNotes(t, st, 1)

	delivered := live.waitN(t, 2)
	if got := rowIDsOf(delivered); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("revoked live batch delivered prefix %v, want rows [1 2]", got)
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenRevoked) {
			t.Fatalf("revocation cause = %v, want ErrListenRevoked", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revocation never closed the session after the prefix")
	}
	time.Sleep(100 * time.Millisecond)
	if n := live.count(); n != 2 {
		t.Fatalf("%d records delivered after the close, want exactly the 2-record prefix", n)
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

// TestListenFilteredFullPageKeepsPaging: exhaustion keys on the scanned
// range, not the admitted page — a full page whose every record the live
// authorization filtered out is not the replay boundary, and ending replay
// there would strand the caller's own visible records behind a page it can
// never turn (§6.2: replay pages are filtered through liveAuthz exactly
// like live delivery).
func TestListenFilteredFullPageKeepsPaging(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	// A full page of foreign records, then the subscriber's own — seeded
	// out-of-band, because no public write path stamps change-record owners
	// until 9c.
	owners := make([]string, 0, MaxChangesPageLimit+2)
	for i := 0; i < MaxChangesPageLimit; i++ {
		owners = append(owners, "bob")
	}
	owners = append(owners, "alice", "alice")
	seedOwnerChanges(t, st, "test", "notes", owners)

	scoped := func(string) (*RowScope, Incarnation, bool) {
		return &RowScope{Owner: "alice"}, Incarnation{}, true
	}
	replay, cancel, err := st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, scoped, func(ChangeRecord) {}, nil)
	if err != nil {
		t.Fatalf("scoped listen: %v", err)
	}
	defer cancel()

	// Page one admits nothing but is FULL — done must stay false.
	records, _, done, err := replay.Next(ctx)
	if err != nil {
		t.Fatalf("filtered page one: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("filtered page one admitted %d records, want 0", len(records))
	}
	if done {
		t.Fatal("a fully filtered FULL page reported done — the caller's own records behind it would be stranded")
	}
	// Page two carries alice's records; the following call reports the
	// boundary.
	records, _, done, err = replay.Next(ctx)
	if err != nil {
		t.Fatalf("page two: %v", err)
	}
	if got := rowIDsOf(records); len(got) != 2 {
		t.Fatalf("page two = %v, want alice's 2 records", got)
	}
	if done {
		t.Fatal("the final records' page reported done — the FOLLOWING call reports the boundary")
	}
	if _, _, done, err = replay.Next(ctx); err != nil || !done {
		t.Fatalf("boundary call = err %v, done %v, want nil, true", err, done)
	}
}

// TestListenZeroBoundaryIsBounded: registering over an EMPTY log fixes the
// boundary at 0 — a real bound, not "unbounded". A commit landing before the
// caller's first Next is live-only (the filler already queued it), and the
// replay half must deliver nothing: the boundary is what keeps the two
// halves disjoint (§6.2's exactly-once).
func TestListenZeroBoundaryIsBounded(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	live := newLiveLog()
	replay, cancel := listenOn(t, st, "", "", live.notify, nil)
	defer cancel()

	fresh := insertNotes(t, st, 2) // commits before the first Next call
	records, _, done, err := replay.Next(ctx)
	if err != nil {
		t.Fatalf("replay Next: %v", err)
	}
	if len(records) != 0 || !done {
		t.Fatalf("replay over a zero boundary delivered %d records, done=%v, want 0, true — the pre-Next commits are live-only", len(records), done)
	}
	delivered := live.waitN(t, 2)
	if got := rowIDsOf(delivered); got[0] != fresh.Ids[0] || got[1] != fresh.Ids[1] {
		t.Fatalf("live delivered %v, want %v exactly once", got, fresh.Ids)
	}
}

// TestListenChainRotatesBeforeCap: a session held past its chain's absolute
// cap (chain_start + 2R, §9.3) rotates to a fresh chain rooted at the
// current position — tokens minted on a capped chain are born expired, and
// every cursor the stream delivers must stay resolvable.
func TestListenChainRotatesBeforeCap(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	live := newLiveLog()
	replay, cancel := listenOn(t, st, "", "", live.notify, nil)
	defer cancel()
	drainReplay(t, replay)

	time.Sleep(150 * time.Millisecond) // past chain_start + 2R
	insertNotes(t, st, 1)
	rec := live.waitN(t, 1)[0]
	cancel()

	if _, _, err := st.ChangesSince(ctx, "test", "", rec.Cursor, [16]byte{}, nil, Incarnation{}, Page{}); err != nil {
		t.Fatalf("the delivered cursor no longer resolves (a capped chain minted a dead token): %v", err)
	}
}

// TestListenCloseEndsSessions: Store.Close wakes every idle session — the
// engine-shutdown close §6.2 promises — instead of stranding sleeping pumps
// and stream clients on pools that will never serve another commit.
func TestListenCloseEndsSessions(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	closedCause := make(chan error, 1)

	replay, cancel, err := st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, nil, newLiveLog().notify,
		func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()
	drainReplay(t, replay)

	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenLifetimeEnded) {
			t.Fatalf("close cause = %v, want ErrListenLifetimeEnded", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close never ended the idle session")
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

// TestListenEarlyEndCarriesResumeCursor: a session ended BEFORE its first
// page — cancelled by its caller, or ended by the engine (an overflow of the
// subscriber's own traffic, a dropped feed target) — still hands back a
// resumable boundary: the standing cursor is fixed at registration, so the
// position after the terminal teaches an exact resume instead of a
// cursorless reconnect that restarts at the head and skips every commit
// after registration.
func TestListenEarlyEndCarriesResumeCursor(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 2)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	cancel() // ends the session before its first page

	_, resume, done, err := replay.Next(context.Background())
	if err != nil {
		t.Fatalf("early-end Next: %v", err)
	}
	if !done {
		t.Fatal("early-end Next reported done=false, want true")
	}
	if resume == "" {
		t.Fatal("early-end Next carried no resume cursor — a session ended before its first page must still teach an exact resume")
	}
	// The cursor is real: a fresh session registered from it replays the
	// full backlog the ended session never delivered.
	replay2, cancel2 := listenOn(t, st, "", resume, func(ChangeRecord) {}, nil)
	defer cancel2()
	if got := rowIDsOf(drainReplay(t, replay2)); len(got) != 2 || got[0] != backlog.Ids[0] || got[1] != backlog.Ids[1] {
		t.Fatalf("resume from the early-end cursor replayed %v, want the full backlog %v", got, backlog.Ids)
	}
}

// TestListenReplayOutlivesRetention: a replay left idle past its chain's
// retention cap fails LOUDLY, never silently short. Another reader's prune
// has since deleted the aged backlog (retention moves with the reads; no
// session pins the log forever, §9.3); the next page reports the
// cursor-expiry teaching error instead of a short page reported as done —
// done would silently omit records the registration boundary promised.
func TestListenReplayOutlivesRetention(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	insertNotes(t, st, 3)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()

	time.Sleep(150 * time.Millisecond) // the backlog ages past 2R; the session's chains expire
	// Another reader moves retention forward: its changes_since prunes the
	// now age-eligible backlog out from under the idle replay.
	if _, _, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{}); err != nil {
		t.Fatalf("changes_since: %v", err)
	}

	if _, _, _, err := replay.Next(ctx); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("aged-out replay Next = %v, want ErrCursorExpired — a pruned backlog must fail loudly, not page short and report done", err)
	}
}

// TestListenDeliversCrossInstanceCommits: the commit registry only wakes
// for commits through THIS store instance, while the persisted concurrency
// guards support another Store — another dolmen process — writing the same
// namespace database. Its commits leave no wake here, so the session's poll
// fallback must re-read the durable log and deliver them (§9.3: the log,
// never the wake, is the delivery guarantee).
func TestListenDeliversCrossInstanceCommits(t *testing.T) {
	st := openChangeStore(t)
	st2, err := Open(st.dir)
	if err != nil {
		t.Fatalf("open second store on %s: %v", st.dir, err)
	}
	t.Cleanup(func() { st2.Close() })

	live := newLiveLog()
	replay, cancel := listenOn(t, st, "", "", live.notify, nil)
	defer cancel()
	drainReplay(t, replay)

	// Commits through the SECOND instance: no in-process wake fires for the
	// first store's registry — only the poll fallback can find them.
	fresh, err := insertNotesErr(st2, 2)
	if err != nil {
		t.Fatalf("cross-instance insert: %v", err)
	}
	delivered := live.waitN(t, 2)
	if got := rowIDsOf(delivered); len(got) != 2 || got[0] != fresh.Ids[0] || got[1] != fresh.Ids[1] {
		t.Fatalf("cross-instance live delivered %v, want %v", got, fresh.Ids)
	}
}

// TestListenPrunesWithoutVisibleDeliveries: retention moves with the reads —
// including reads that deliver nothing. A subscriber whose entire traffic
// the authorization filters out never mints, so the per-delivery prune never
// runs; the fill's own reads must prune, or the invisible stream's traffic
// would accumulate without bound under a nonzero retention (§9.3). No
// further commit is needed after aging: the poll fallback's reads carry the
// prune.
func TestListenPrunesWithoutVisibleDeliveries(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	empty := newLiveLog()
	emptyScope := func(string) (*RowScope, Incarnation, bool) {
		return &RowScope{Empty: true}, Incarnation{}, true
	}
	replay, cancel, err := st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, emptyScope, empty.notify, nil)
	if err != nil {
		t.Fatalf("empty-scope listen: %v", err)
	}
	defer cancel()
	drainReplay(t, replay)

	insertNotes(t, st, 3)
	time.Sleep(150 * time.Millisecond) // the records age past 2R; the session's chains expire

	deadline := time.Now().Add(5 * time.Second)
	for {
		n, nerr := st.ns("test")
		if nerr != nil {
			t.Fatalf("open test ns: %v", nerr)
		}
		var count int
		if qerr := n.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM _dolmen_changes`).Scan(&count); qerr != nil {
			t.Fatalf("count changes: %v", qerr)
		}
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d change records survived 2R under a live all-invisible subscriber — retention must move with the reads", count)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := empty.count(); n != 0 {
		t.Fatalf("empty-scope subscriber delivered %d records, want 0", n)
	}
}
// seedStampedChanges seeds change records with explicit at stamps — the
// out-of-band fixture for non-monotonic stamping (a clock step between
// commits), which no public write path can produce: retention deletes by
// age, and only a stamp older than a LATER seq's makes pruning delete an
// interior record.
func seedStampedChanges(t *testing.T, st *Store, table string, ats []time.Time) {
	t.Helper()
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	nsGen, err := readNSGen(ctx, tx)
	if err != nil {
		t.Fatalf("read nsgen: %v", err)
	}
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		t.Fatalf("read drop gen: %v", err)
	}
	for i, at := range ats {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO _dolmen_changes(table_name, row_id, kind, owner, nsgen, drop_gen, at) VALUES(?,?,?,?,?,?,?)`,
			table, int64(i+1), string(ChangeInsert), nil, nsGen[:], gen, isoChangeStamp(at)); err != nil {
			t.Fatalf("seed change row %d: %v", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestListenReplayInteriorHoleFailsLoudly: pruning deletes by age, and at
// stamps are not seq-ordered after a clock step — so an aged MIDDLE record
// can be deleted behind fresher neighbors, and the oldest-record check
// alone would accept the log and silently skip the missing seq. The page
// verifies every seq in the span it consumed; a hole is the same
// cursor-expiry teaching error, never a delivered gap.
func TestListenReplayInteriorHoleFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// Seq 2 carries an at stamp 200ms in the past (a clock step); seqs 1
	// and 3 are fresh. Only seq 2 ever ages past 2R.
	seedStampedChanges(t, st, "notes", []time.Time{time.Now(), time.Now().Add(-200 * time.Millisecond), time.Now()})

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()
	time.Sleep(150 * time.Millisecond) // the session's chains expire; seq 2 becomes prunable
	if _, _, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{}); err != nil {
		t.Fatalf("changes_since: %v", err)
	}

	if _, _, _, err := replay.Next(ctx); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("holey replay Next = %v, want ErrCursorExpired — an interior hole must fail loudly, not skip the missing seq", err)
	}
}

// TestListenReplayMissingTailFailsLoudly: the empty-scan blind spot of the
// hole checks — pruning can remove every outstanding row while a
// future-stamped row at or before the position survives (a clock step), so
// the head check passes, the scan pages nothing, and the span COUNT never
// ran. The terminal empty page verifies the whole outstanding range
// exactly: rows missing from the promised tail fail loudly, never a done
// that silently omits them.
func TestListenReplayMissingTailFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// Seq 1 is stamped 100ms AHEAD of the seed (a clock step): at prune time
	// it is younger than the 2R deletion cutoff (survives) yet older than
	// the R window (so the pruning reader's empty window roots its chain at
	// the head, freeing seqs 2 and 3 for deletion behind it).
	seedStampedChanges(t, st, "notes", []time.Time{time.Now().Add(100 * time.Millisecond), time.Now().Add(-200 * time.Millisecond), time.Now().Add(-200 * time.Millisecond)})

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()
	time.Sleep(150 * time.Millisecond) // the session's chains expire; seqs 2-3 become prunable
	if _, _, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{}); err != nil {
		t.Fatalf("changes_since: %v", err)
	}

	// The first page is short (one survivor) and the promised tail is gone:
	// the page must say so instead of reporting the replay complete.
	if _, _, _, err := replay.Next(ctx); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("missing-tail Next = %v, want ErrCursorExpired — a pruned tail must fail loudly, not page short and report done", err)
	}
}

// TestListenMintsOnFreshTime: the mint runs on the time it mints, not the
// page's start — the read and the admission callbacks can span the chain's
// retention cap, and a stale timestamp would keep the already-expired chain
// and stamp born-dead cursors an immediate resume rejects.
func TestListenMintsOnFreshTime(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	insertNotes(t, st, 2)

	// Admission slower than the chain cap: the page starts before
	// chain_start+2R and mints well past it.
	slow := func(string) (*RowScope, Incarnation, bool) {
		time.Sleep(60 * time.Millisecond)
		return nil, Incarnation{}, true
	}
	replay, cancel, err := st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, slow, func(ChangeRecord) {}, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()
	for _, rec := range drainReplay(t, replay) {
		if _, _, err := st.ChangesSince(ctx, "test", "", rec.Cursor, [16]byte{}, nil, Incarnation{}, Page{}); err != nil {
			t.Fatalf("cursor minted across the chain cap does not resolve: %v", err)
		}
	}
}

// TestListenBeginAcceptsPreExistingGaps: the loss check is a BASELINE, not
// span arithmetic. A clock step can leave a hole BEFORE registration (a
// future-stamped survivor beside aged neighbors, pruned by a reader whose
// chain roots past them); a begin registration selects the first retained
// record and legitimately replays AROUND that hole — every retained record
// in (P, R] delivers, no teaching error, because only rows removed AFTER
// the boundary was fixed are the session's to lose.
func TestListenBeginAcceptsPreExistingGaps(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// Seq 1 is stamped an hour ahead and seq 4 a beat ahead (both survive the
	// prune and sit in the retention window); seqs 2 and 3 are aged. A bare
	// reader after the chains expire roots
	// its chain at the head, freeing seqs 2 and 3 for deletion behind the
	// future-stamped survivor — a pre-existing hole between 1 and 4.
	seedStampedChanges(t, st, "notes", []time.Time{
		time.Now().Add(time.Hour),
		time.Now().Add(-200 * time.Millisecond),
		time.Now().Add(-200 * time.Millisecond),
		time.Now().Add(200 * time.Millisecond),
	})
	time.Sleep(150 * time.Millisecond)
	if _, _, err := st.ChangesSince(ctx, "test", "", "", [16]byte{}, nil, Incarnation{}, Page{}); err != nil {
		t.Fatalf("changes_since: %v", err)
	}

	// begin registration: P floors at the first retained record (seq 1),
	// the hole at 2-3 sits INSIDE (P, R], and the replay must deliver
	// exactly the retained rows around it.
	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()
	if got := rowIDsOf(drainReplay(t, replay)); len(got) != 2 || got[0] != 1 || got[1] != 4 {
		t.Fatalf("gap-tolerant replay delivered %v, want the retained rows [1 4] around the pre-existing hole", got)
	}
}

// TestListenQueuedRecordsSurviveSlowSubscriber: a subscriber blocked past
// the chain cap must not have its buffered backlog pruned out from under
// the queue — the fill rotates the chain to root at the queue's head, so
// the poll-path prunes cannot delete rows the drainer has not yet
// delivered. Without that protection a delivered cursor would resolve over
// a log missing the records between deliveries, breaking the gap-free
// reconnect.
func TestListenQueuedRecordsSurviveSlowSubscriber(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// The slowest possible subscriber: nothing is read until released.
	block := make(chan struct{})
	live := newLiveLog()
	replay, cancel := listenOn(t, st, "", CursorBegin, func(rec ChangeRecord) {
		<-block
		live.notify(rec)
	}, nil)
	defer cancel()
	drainReplay(t, replay)

	insertNotes(t, st, 3)
	time.Sleep(150 * time.Millisecond) // past 2R: the queue's rows are age-eligible, the poll-path prunes run
	close(block)

	delivered := live.waitN(t, 3) // every queued record arrives, minted at its own delivery
	// The records minted AFTER the unblock ride the freshly rotated chain
	// and must resolve over an intact remainder. (The first record's cursor
	// was minted just before the block and legitimately dies with its chain
	// at 2R — the documented retention bound, a loud teaching error rather
	// than a silent gap. Rows at or below the rotation's origin are
	// prunable after delivery — retention moves with the reads — so the
	// pin is the exact remainder each cursor promises, not the row count.)
	for _, rec := range delivered[1:] {
		data, _, err := st.ChangesSince(ctx, "test", "", rec.Cursor, [16]byte{}, nil, Incarnation{}, Page{})
		if err != nil {
			t.Fatalf("delivered cursor does not resolve: %v", err)
		}
		want := int(delivered[2].RowID - rec.RowID)
		if len(data) != want {
			t.Fatalf("reconnect from the delivered cursor for row %d sees %d changes, want %d — the queue's rows were pruned out from under the drainer", rec.RowID, len(data), want)
		}
	}
}

// TestListenRevocationStopsLaterFills: the first ok=false is terminal — a
// parked close must also stop the fills, so records landing after the
// revocation never deliver even when access is re-granted a beat later and
// liveAuthz would pass again.
func TestListenRevocationStopsLaterFills(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	revoked := false
	authz := func(string) (*RowScope, Incarnation, bool) {
		if revoked {
			return nil, Incarnation{}, false
		}
		return nil, Incarnation{}, true
	}
	live := newLiveLog()
	closedCause := make(chan error, 1)
	replay, cancel, err := st.Listen(ctx, "test", "", "", [16]byte{}, authz, live.notify,
		func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()
	drainReplay(t, replay)

	revoked = true
	insertNotes(t, st, 1) // the batch that finds the revocation
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenRevoked) {
			t.Fatalf("close cause = %v, want ErrListenRevoked", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revocation never closed the session")
	}

	// Access returns; the session must not come back with it.
	revoked = false
	insertNotes(t, st, 2)
	time.Sleep(300 * time.Millisecond) // poll falls, fills would run if not gated
	if n := live.count(); n != 0 {
		t.Fatalf("%d records delivered after the revocation close (access re-granted), want 0 — the first ok=false is terminal", n)
	}
}

// TestListenQueueProtectionIsDurable: the queue's chain protection must be
// PERSISTED — the prune's guard reads token rows, so an in-memory rotation
// protects nothing. While the subscriber is blocked past 2R, the poll-path
// prunes run and the rows must still be there.
func TestListenQueueProtectionIsDurable(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	block := make(chan struct{})
	live := newLiveLog()
	replay, cancel := listenOn(t, st, "", CursorBegin, func(rec ChangeRecord) {
		<-block
		live.notify(rec)
	}, nil)
	defer cancel()
	drainReplay(t, replay)

	insertNotes(t, st, 3)
	time.Sleep(150 * time.Millisecond) // past 2R, several poll-and-prune rounds

	// While the subscriber is still blocked: the rows must have survived.
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	var count int
	if err := n.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM _dolmen_changes`).Scan(&count); err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if count != 3 {
		t.Fatalf("%d of 3 change rows survived the blocked subscriber — the queue's protection was not durable", count)
	}
	close(block)
	if got := len(live.waitN(t, 3)); got != 3 {
		t.Fatalf("%d records delivered, want all 3", got)
	}
}
