package store

import (
	"context"
	"testing"
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
