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

func TestListenCancelUntracks(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 1)

	replay, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, nil)
	drainReplay(t, replay)
	if got := len(st.listenSessions["test"]); got != 1 {
		t.Fatalf("tracked sessions = %d, want 1", got)
	}
	cancel()
	if got := len(st.listenSessions["test"]); got != 0 {
		t.Fatalf("tracked sessions after cancel = %d, want 0", got)
	}
}

func TestListenFailedRegistrationUntracks(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 1)

	if _, _, err := st.Listen(context.Background(), "test", "", Cursor("not-a-token"), [16]byte{}, nil, func(ChangeRecord) {}, nil); err == nil {
		t.Fatal("listen with a garbage cursor succeeded")
	}
	if got := len(st.listenSessions["test"]); got != 0 {
		t.Fatalf("tracked sessions after failed registration = %d, want 0", got)
	}
	if got := len(st.listeners["test"]); got != 0 {
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
