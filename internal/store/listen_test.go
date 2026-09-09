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
