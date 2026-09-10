package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Slice 6b, replay half: Listen's registration and ChangeReplay paging. The
// fixtures drive the real write paths (like the 5c suite) and observe the
// replay exactly as a caller would: pages through ChangeReplay.Next, engine
// ends through closed. The live half — notify delivery after the drained
// boundary, the overflow close — is 6b's next change and gets its own
// fixtures there.

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

// TestListenReplayBacklog: the replay half's core shape — a session holding
// a cursor replays exactly the backlog after it, each record carrying a
// fresh cursor, and the following call reports the registration boundary
// (§6.2, §9.3). The live half (6b's next change) continues from the drained
// boundary; until it lands, notify is never invoked.
func TestListenReplayBacklog(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 3)

	live := false
	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) { live = true }, nil)
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
	if live {
		t.Fatal("notify invoked during the replay half — delivery is the live half's (6b's next change)")
	}
}

// TestListenEarlyEndCarriesResumeCursor: a session ended BEFORE its first
// page — cancelled here; once the live half lands, an engine-initiated end
// (an overflow of the subscriber's own traffic) hits the same path — still
// hands back a resumable boundary: the standing cursor is fixed at
// registration, so the terminal position teaches an exact resume instead of
// a cursorless reconnect that restarts at the head and skips every commit
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

// TestListenBareStartEmptyReplay: the zero cursor fixes the boundary at the
// current head — the replay is empty (first Next reports done immediately);
// only subsequent commits would arrive, and they are the live half's (§9.3's
// wake-up semantics).
func TestListenBareStartEmptyReplay(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 3)

	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, nil)
	defer cancel()

	records, _, done, err := replay.Next(context.Background())
	if err != nil {
		t.Fatalf("bare-start Next: %v", err)
	}
	if len(records) != 0 || !done {
		t.Fatalf("bare-start replay = %d records, done=%v, want 0, true", len(records), done)
	}
}

// TestListenZeroBoundaryIsBounded: registering over an EMPTY log fixes the
// boundary at 0 — a real bound, not "unbounded". A commit landing before the
// caller's first Next is past the boundary, and the replay half must deliver
// nothing: the boundary is what keeps the two halves disjoint (§6.2's
// exactly-once), and replaying it here would duplicate it once the live half
// delivers it.
func TestListenZeroBoundaryIsBounded(t *testing.T) {
	st := openChangeStore(t)

	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, nil)
	defer cancel()

	insertNotes(t, st, 2) // commits before the first Next call
	records, _, done, err := replay.Next(context.Background())
	if err != nil {
		t.Fatalf("replay Next: %v", err)
	}
	if len(records) != 0 || !done {
		t.Fatalf("replay over a zero boundary delivered %d records, done=%v, want 0, true — the pre-Next commits are past the boundary", len(records), done)
	}
}

// TestListenReplayAuthzAdmission: per-event authorization runs BEFORE a
// record is exposed (§6.2): a scope filters by the record's Owner label, an
// Empty scope sees nothing, and ok=false teaching-closes the stream with the
// page omitted — closed precedes every exposed record (a consumer may tear
// down in its closed callback). Owner-labeled records are seeded out-of-band
// (seedOwnerChanges): no public write path stamps the change record's owner
// until slice 9c.
func TestListenReplayAuthzAdmission(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	seedOwnerChanges(t, st, "test", "notes", []string{"alice", "alice", "bob"})

	// A scoped viewer sees exactly their own rows.
	scoped := func(string) (*RowScope, Incarnation, bool) {
		return &RowScope{Owner: "alice"}, Incarnation{}, true
	}
	replay, cancel, err := st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, scoped, func(ChangeRecord) {}, nil)
	if err != nil {
		t.Fatalf("scoped listen: %v", err)
	}
	if got := rowIDsOf(drainReplay(t, replay)); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("scoped replay delivered %v, want alice's rows [1 2] only", got)
	}
	cancel()

	// An Empty scope sees nothing at all.
	emptyScope := func(string) (*RowScope, Incarnation, bool) {
		return &RowScope{Empty: true}, Incarnation{}, true
	}
	replay, cancel, err = st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, emptyScope, func(ChangeRecord) {}, nil)
	if err != nil {
		t.Fatalf("empty-scope listen: %v", err)
	}
	defer cancel()
	if got := rowIDsOf(drainReplay(t, replay)); len(got) != 0 {
		t.Fatalf("empty-scope replay delivered %v, want nothing", got)
	}

	// ok=false mid-replay is a teaching close that precedes every exposed
	// record: the page carrying the revoked record is omitted entirely —
	// closed fires with the cause, Next reports done, and the omitted
	// records re-deliver on the caller's reconnect from its last cursor.
	seen := 0
	revokeAfterFirst := func(string) (*RowScope, Incarnation, bool) {
		seen++
		if seen > 1 {
			return nil, Incarnation{}, false
		}
		return nil, Incarnation{}, true
	}
	closedCause := make(chan error, 1)
	replay, cancel, err = st.Listen(ctx, "test", "", CursorBegin, [16]byte{}, revokeAfterFirst, func(ChangeRecord) {},
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
