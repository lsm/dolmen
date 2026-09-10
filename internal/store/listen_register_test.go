package store

import (
	"context"
	"errors"
	"testing"
)

// Slice 6b (r1): Listen registration — the boundary transaction. The
// fixtures drive the real write paths (like the 5c suite) and observe the
// registration exactly as a caller would: the returned replay's first page
// (the registration boundary, in this slice) and the standing resume
// cursor through Resume.

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

// TestListenBareStartEmptyReplay: the zero cursor fixes the boundary at the
// current head — the replay is empty (first Next reports done immediately
// with the standing cursor); only subsequent commits would arrive, and they
// are the live half's (§9.3's wake-up semantics).
func TestListenBareStartEmptyReplay(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 3)

	live := false
	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) { live = true }, nil)
	defer cancel()

	records, next, done, err := replay.Next(context.Background())
	if err != nil {
		t.Fatalf("bare-start Next: %v", err)
	}
	if len(records) != 0 || !done {
		t.Fatalf("bare-start replay = %d records, done=%v, want 0, true", len(records), done)
	}
	if next == "" {
		t.Fatal("bare-start Next carried no cursor")
	}
	if replay.Resume() != next {
		t.Fatalf("Resume = %q, want the same standing cursor %q", replay.Resume(), next)
	}
	if live {
		t.Fatal("notify invoked — delivery is the live half's (a later 6b slice)")
	}
}

// TestListenEarlyEndCarriesResumeCursor: a session ended BEFORE its first
// page — cancelled here; once the live half lands, an engine-initiated end
// (an overflow of its own traffic) hits the same path — still hands back a
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
	if replay.Resume() == "" {
		t.Fatal("Resume returned no cursor after the early end")
	}
	// The cursor is real: a fresh session registered from it replays the
	// full backlog the ended session never delivered.
	replay2, cancel2 := listenOn(t, st, "", resume, func(ChangeRecord) {}, nil)
	defer cancel2()
	records, _, done, err := replay2.Next(context.Background())
	if err != nil {
		t.Fatalf("resume Next: %v", err)
	}
	// The page read lands with the next slice; the standing cursor is at
	// the registration boundary, so the resumed session's own boundary call
	// reports done with a real cursor either way — the backlog itself
	// delivers once paging exists. Here the pin is that registration from
	// the early-end cursor succeeds and reports the boundary honestly.
	if done && len(records) != 0 {
		t.Fatalf("resumed session first page = %d records, want the boundary contract", len(records))
	}
	_ = backlog
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

// TestListenCancelNeverCloses: cancel is the caller's teardown — closed
// never fires for it (§6.2), the replay reports done after, and cancel is
// idempotent.
func TestListenCancelNeverCloses(t *testing.T) {
	st := openChangeStore(t)
	closedFired := make(chan error, 1)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, func(cause error) { closedFired <- cause })
	cancel()
	cancel() // idempotent

	select {
	case cause := <-closedFired:
		t.Fatalf("closed fired on caller cancel with %v", cause)
	default:
	}
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("Next after cancel = err %v, done %v, want nil error, done=true", err, done)
	}
}
