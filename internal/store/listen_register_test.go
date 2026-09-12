package store

import (
	"context"
	"errors"
	"testing"
)

func listenOn(t *testing.T, st *Store, table string, from Cursor, notify func(ChangeRecord), closed func(error)) (*ChangeReplay, func()) {
	t.Helper()
	replay, cancel, err := st.Listen(context.Background(), "test", table, from, [16]byte{}, nil, notify, closed)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return replay, cancel
}

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

func TestListenEarlyEndCarriesResumeCursor(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 2)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	cancel()

	_, resume, done, err := replay.Next(context.Background())
	if !errors.Is(err, errListenEnded) {
		t.Fatalf("early-end Next = %v, want the ended marker — clean done must mean provably alive", err)
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

	replay2, cancel2 := listenOn(t, st, "", resume, func(ChangeRecord) {}, nil)
	defer cancel2()
	records, _, done, err := replay2.Next(context.Background())
	if err != nil {
		t.Fatalf("resume Next: %v", err)
	}

	if done && len(records) != 0 {
		t.Fatalf("resumed session first page = %d records, want the boundary contract", len(records))
	}
	_ = backlog
}

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

func TestListenCancelNeverCloses(t *testing.T) {
	st := openChangeStore(t)
	closedFired := make(chan error, 1)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, func(cause error) { closedFired <- cause })
	cancel()
	cancel()

	select {
	case cause := <-closedFired:
		t.Fatalf("closed fired on caller cancel with %v", cause)
	default:
	}
	if _, _, done, err := replay.Next(context.Background()); !errors.Is(err, errListenEnded) || !done {
		t.Fatalf("Next after cancel = err %v, done %v, want the ended marker and done=true", err, done)
	}
}
