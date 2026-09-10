package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Slice 6b (r3): the replay-page read. The fixtures drive the real write
// paths (like the 5c suite) and page the replay exactly as a caller would:
// records through Next, the done contract, cursors minted per record.

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

// TestListenReplayBacklog: a session holding a cursor replays exactly the
// backlog after it, each record carrying a fresh cursor, and the following
// call reports the registration boundary (§6.2, §9.3). The live half (a
// later slice) continues from the drained boundary; until then notify is
// never invoked.
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
		t.Fatal("notify invoked during the replay half — delivery is the live half's (a later slice)")
	}
}

// TestListenPagedReplayDoneContract pins ChangeReplay's done protocol
// exactly: pages of MaxChangesPageLimit, the page carrying the final records
// reports done=false, the following call reports done=true with no records —
// the registration boundary (§6.2).
func TestListenPagedReplayDoneContract(t *testing.T) {
	st := openChangeStore(t)
	n := 2*MaxChangesPageLimit + 500
	for n > 0 { // chunked to the per-call record cap
		size := n
		if size > MaxRecordsPerInsert {
			size = MaxRecordsPerInsert
		}
		records := make([]map[string]any, size)
		for i := range records {
			records[i] = map[string]any{"title": "a", "score": i + 1}
		}
		if _, err := st.Insert(context.Background(), "test", "notes", records, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		n -= size
	}

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

// TestListenBareStartSkipsBacklog: the zero cursor fixes the boundary at the
// current head, so the backlog before registration is never replayed —
// only subsequent commits would arrive, and they are the live half's (§9.3's
// wake-up semantics).
func TestListenBareStartSkipsBacklog(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 3)

	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, nil)
	defer cancel()

	if got := rowIDsOf(drainReplay(t, replay)); len(got) != 0 {
		t.Fatalf("bare-start replay delivered %v, want nothing — the backlog predates the head boundary", got)
	}
}

// TestListenZeroBoundaryIsBounded: registering over an EMPTY log fixes the
// boundary at 0 — a real bound, not "unbounded". A commit landing before the
// caller's first Next is past the boundary, and the replay half must deliver
// nothing: the boundary is what keeps the two halves disjoint (§6.2's
// exactly-once).
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

// TestListenResumeCursorReplaysBacklog: a session registered from a token
// cursor replays the backlog after that position — the resume the standing
// cursor teaches.
func TestListenResumeCursorReplaysBacklog(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 2)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	cancel() // the standing cursor is fixed at registration; Next after cancel still teaches it
	_, resume, _, err := replay.Next(context.Background())
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if resume == "" {
		t.Fatal("no resume cursor from the first page")
	}

	replay2, cancel2 := listenOn(t, st, "", resume, func(ChangeRecord) {}, nil)
	defer cancel2()
	if got := rowIDsOf(drainReplay(t, replay2)); len(got) != 2 || got[0] != backlog.Ids[0] || got[1] != backlog.Ids[1] {
		t.Fatalf("resume replayed %v, want the full backlog %v", got, backlog.Ids)
	}
}

// TestListenNextSingleFlight: concurrent Next calls on one session are
// serialized as page+publish units — every record delivers exactly once,
// and the published position never regresses. Without the flight lock, two
// callers scan the same sess.position and duplicate the page; a slower
// earlier caller then publishes its position over a later one's.
func TestListenNextSingleFlight(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 20)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()

	const callers = 4
	var wg sync.WaitGroup
	results := make([][]ChangeRecord, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				records, _, done, err := replay.Next(context.Background())
				if err != nil {
					t.Errorf("caller %d Next: %v", i, err)
					return
				}
				results[i] = append(results[i], records...)
				if done {
					return
				}
			}
		}(i)
	}
	wg.Wait()

	seen := map[int64]int{}
	for _, r := range results {
		for _, rec := range r {
			seen[rec.RowID]++
		}
	}
	if len(seen) != 20 {
		t.Fatalf("concurrent paging delivered %d distinct rows, want all 20", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("row %d delivered %d times across concurrent Next calls, want exactly 1", id, n)
		}
	}
}

// seedStampedChanges seeds change records with explicit at stamps — the
// fixture for clock-stepped pruning scenarios, which no public write path
// can produce.
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

// openStampedStore is the retention-store fixture for the loss tests: a
// 40ms retention, the test namespace, and the notes table.
func openStampedStore(t *testing.T) *Store {
	t.Helper()
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
	return st
}

// pruneWithReader runs a bare changes_since — a reader whose mint roots a
// fresh chain at the head, freeing the aged rows behind it for deletion.
func pruneWithReader(t *testing.T, st *Store) {
	t.Helper()
	if _, _, err := st.ChangesSince(context.Background(), "test", "", "", [16]byte{}, nil, Incarnation{}, Page{}); err != nil {
		t.Fatalf("changes_since: %v", err)
	}
}

// TestListenReplayOutlivesRetention: a replay left idle past its chain's
// retention cap fails LOUDLY, never silently short. Another reader's prune
// has deleted the aged backlog; the next page reports the cursor-expiry
// teaching error instead of a short page reported as done.
func TestListenReplayOutlivesRetention(t *testing.T) {
	st := openStampedStore(t)
	insertNotes(t, st, 3)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()
	time.Sleep(150 * time.Millisecond) // chains expire; rows age past 2R
	pruneWithReader(t, st)

	if _, _, _, err := replay.Next(context.Background()); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("aged-out replay Next = %v, want ErrCursorExpired — a pruned backlog must fail loudly, not page short and report done", err)
	}
}

// TestListenReplayInteriorHoleFailsLoudly: pruning deletes by age, and at
// stamps are not seq-ordered after a clock step — an aged MIDDLE record can
// be deleted behind fresher neighbors, and a span count over the scanned
// range alone would be tautological. The full-tail entry check catches the
// hole before the page serves past it.
func TestListenReplayInteriorHoleFailsLoudly(t *testing.T) {
	st := openStampedStore(t)
	// Seqs 1 and 3 are stamped ahead (survive every prune, in-window); only
	// the MIDDLE seq 2 is aged. A bare reader's prune deletes exactly seq 2,
	// leaving surviving rows on both sides — the interior-hole shape.
	seedStampedChanges(t, st, "notes", []time.Time{
		time.Now().Add(100 * time.Millisecond),
		time.Now().Add(-200 * time.Millisecond),
		time.Now().Add(100 * time.Millisecond),
	})

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()
	time.Sleep(150 * time.Millisecond)
	pruneWithReader(t, st)

	if _, _, _, err := replay.Next(context.Background()); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("holey replay Next = %v, want ErrCursorExpired — an interior hole must fail loudly, not skip the missing seq", err)
	}
}

// TestListenReplayMissingTailFailsLoudly: pruning removes the aged TAIL rows
// while a future-stamped head row survives, so the scan still finds the
// head and a span count over the scanned range stays tautological. The
// full-tail entry count sees the loss: the baseline promised three, the
// tail holds one.
func TestListenReplayMissingTailFailsLoudly(t *testing.T) {
	st := openStampedStore(t)
	// Seq 1 future-stamped (in-window at registration, survives every
	// prune — begin resolves P below it); seqs 2-3 aged, prunable behind
	// the survivor.
	seedStampedChanges(t, st, "notes", []time.Time{
		time.Now().Add(100 * time.Millisecond),
		time.Now().Add(-200 * time.Millisecond),
		time.Now().Add(-200 * time.Millisecond),
	})

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()
	time.Sleep(150 * time.Millisecond)
	pruneWithReader(t, st)

	if _, _, _, err := replay.Next(context.Background()); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("missing-tail Next = %v, want ErrCursorExpired — a pruned tail must fail loudly, not report done", err)
	}
}

// TestListenChainRotatesBeforeCap: a session held past its chain's absolute
// cap (chain_start + 2R, §9.3) rotates to a fresh chain rooted at the
// current position — tokens minted on a capped chain are born expired, and
// every cursor the replay delivers must stay resolvable.
func TestListenChainRotatesBeforeCap(t *testing.T) {
	st := openStampedStore(t)
	ctx := context.Background()
	insertNotes(t, st, 2)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()

	time.Sleep(150 * time.Millisecond) // past chain_start + 2R; no prune ran
	replayed := drainReplay(t, replay)
	if len(replayed) != 2 {
		t.Fatalf("replay delivered %d records, want the 2-record backlog", len(replayed))
	}
	for _, rec := range replayed {
		if _, _, err := st.ChangesSince(ctx, "test", "", rec.Cursor, [16]byte{}, nil, Incarnation{}, Page{}); err != nil {
			t.Fatalf("the delivered cursor no longer resolves (a capped chain minted a dead token): %v", err)
		}
	}
	cancel()
}
