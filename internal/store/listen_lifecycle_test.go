package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

func waitForClose(t *testing.T, closedFired <-chan error, want error) {
	t.Helper()
	select {
	case cause := <-closedFired:
		if !errors.Is(cause, want) {
			t.Fatalf("closed fired with %v, want %v", cause, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("closed never fired with %v", want)
	}
}

func trackedSessions(st *Store, ns string) int {
	st.notifyMu.Lock()
	defer st.notifyMu.Unlock()
	return len(st.listenSessions[ns])
}

func TestListenDropNamespaceEndsSession(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	closedFired := make(chan error, 1)
	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, func(cause error) { closedFired <- cause })
	drainReplay(t, replay)

	if err := st.DropNamespace(context.Background(), "test", [16]byte{}); err != nil {
		t.Fatalf("drop namespace: %v", err)
	}
	waitForClose(t, closedFired, ErrListenLifetimeEnded)
	cancel()
}

func TestListenDropTableEndsTableFeed(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	closedFired := make(chan error, 1)
	replay, cancel := listenOn(t, st, "notes", "", func(ChangeRecord) {}, func(cause error) { closedFired <- cause })
	drainReplay(t, replay)

	if err := st.DropTable(context.Background(), "test", "notes", Incarnation{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	waitForClose(t, closedFired, ErrListenLifetimeEnded)
	cancel()
}

func TestListenStoreCloseEndsSessions(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	closedFired := make(chan error, 1)
	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, func(cause error) { closedFired <- cause })
	drainReplay(t, replay)

	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	waitForClose(t, closedFired, ErrListenLifetimeEnded)
	cancel()
}

func TestListenRecreateEndsPredecessorSession(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	closedFired := make(chan error, 1)
	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, func(cause error) { closedFired <- cause })
	drainReplay(t, replay)

	if err := os.Remove(st.nsPath("test")); err != nil {
		t.Fatalf("remove namespace file out-of-band: %v", err)
	}
	if err := st.CreateNamespace(context.Background(), "test", [16]byte{}); err != nil {
		t.Fatalf("recreate namespace: %v", err)
	}
	waitForClose(t, closedFired, ErrListenLifetimeEnded)
	cancel()
}

func TestListenFailedDropEndsWithEngineCause(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateNamespace(context.Background(), "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	insertNotes(t, st, 1)

	closedFired := make(chan error, 1)
	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, func(cause error) { closedFired <- cause })
	drainReplay(t, replay)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	dropErr := st.DropNamespace(context.Background(), "test", [16]byte{})
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir back: %v", err)
	}
	if dropErr == nil {
		t.Skip("drop succeeded despite the read-only directory — privileges bypass the removal failure this test needs")
	}

	select {
	case cause := <-closedFired:
		if cause == nil {
			t.Fatal("failed drop closed the session with a nil cause")
		}
		if errors.Is(cause, ErrListenLifetimeEnded) {
			t.Fatalf("a namespace that still exists taught the lifetime end: %v", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the failed drop left the session on closed pools with no terminal")
	}
	cancel()

	closedAgain := make(chan error, 1)
	replay2, cancel2 := listenOn(t, st, "", "", func(ChangeRecord) {}, func(cause error) { closedAgain <- cause })
	drainReplay(t, replay2)
	cancel2()
	select {
	case cause := <-closedAgain:
		t.Fatalf("a fresh registration on the surviving namespace closed with %v", cause)
	default:
	}

	if err := st.DropNamespace(context.Background(), "test", [16]byte{}); err != nil {
		t.Fatalf("drop after restoring permissions: %v", err)
	}
}

func TestListenCancelUntracks(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 1)

	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, nil)
	drainReplay(t, replay)
	if got := trackedSessions(st, "test"); got != 1 {
		t.Fatalf("tracked sessions = %d, want 1", got)
	}
	cancel()
	if got := trackedSessions(st, "test"); got != 0 {
		t.Fatalf("tracked sessions after cancel = %d, want 0", got)
	}
}

func TestListenFailedRegistrationUntracks(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 1)

	if _, _, err := st.Listen(context.Background(), "test", "", Cursor("not-a-token"), [16]byte{}, nil, func(ChangeRecord) {}, nil); err == nil {
		t.Fatal("listen with a garbage cursor succeeded")
	}
	if got := trackedSessions(st, "test"); got != 0 {
		t.Fatalf("tracked sessions after failed registration = %d, want 0", got)
	}
	if got := commitListenersOf(st, "test"); got != 0 {
		t.Fatalf("commit listeners after failed registration = %d, want 0", got)
	}
}

func TestFillErrEvictedMapsLifetimeEnded(t *testing.T) {
	st := openChangeStore(t)
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n

	if cause := sess.fillErr(sql.ErrConnDone); errors.Is(cause, ErrListenLifetimeEnded) {
		t.Fatal("a live namespace's read error mapped to the lifetime end")
	}

	if err := st.DropNamespace(context.Background(), "test", [16]byte{}); err != nil {
		t.Fatalf("drop namespace: %v", err)
	}
	if cause := sess.fillErr(sql.ErrConnDone); !errors.Is(cause, ErrListenLifetimeEnded) {
		t.Fatalf("evicted pool error mapped to %v, want ErrListenLifetimeEnded", cause)
	}
}

func commitListenersOf(st *Store, ns string) int {
	st.notifyMu.Lock()
	defer st.notifyMu.Unlock()
	return len(st.listeners[ns])
}
