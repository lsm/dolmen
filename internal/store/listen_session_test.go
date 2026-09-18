package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func testSession(closed func(error)) *listenSession {
	sess := &listenSession{nsName: "test", table: "notes", closedFn: closed}
	sess.cond = sync.NewCond(&sess.mu)
	sess.stop = make(chan struct{})
	sess.ctx, sess.ctxCancel = context.WithCancel(context.Background())
	return sess
}

func TestSessionCancelNeverCloses(t *testing.T) {
	closedFired := make(chan error, 1)
	sess := testSession(func(cause error) { closedFired <- cause })

	sess.cancel()
	sess.cancel()

	select {
	case cause := <-closedFired:
		t.Fatalf("closed fired on caller cancel with %v", cause)
	default:
	}
	if !sess.dead {
		t.Fatal("cancel left the session live")
	}
}

func TestSessionEndFiresClosedOnce(t *testing.T) {
	causes := make(chan error, 4)
	sess := testSession(func(cause error) { causes <- cause })

	boom := errors.New("engine end")
	sess.end(boom)
	sess.end(errors.New("second"))
	sess.cancel()

	if n := len(causes); n != 1 {
		t.Fatalf("closed fired %d times, want exactly once", n)
	}
	if got := <-causes; !errors.Is(got, boom) {
		t.Fatalf("close cause = %v, want %v", got, boom)
	}
}

func TestSessionClosedPanicRecovered(t *testing.T) {
	ran := false
	sess := testSession(func(error) { ran = true; panic("boom") })
	sess.end(errors.New("cause"))

	if !sess.dead {
		t.Fatal("session not dead after the panicking close")
	}
	if !ran {
		t.Fatal("the direct end never fired closedFn — the parked cause had no flusher")
	}
}

func TestSessionCursorIsStanding(t *testing.T) {
	sess := testSession(nil)
	if got := sess.cursor(); got != "" {
		t.Fatalf("zero-value session cursor = %q, want empty", got)
	}
	sess.mu.Lock()
	sess.nextCursor = "tok"
	sess.mu.Unlock()
	if got := sess.cursor(); got != "tok" {
		t.Fatalf("cursor = %q, want the standing token", got)
	}
}

func TestSessionDeathCauseSurvivesParkedCloseFlush(t *testing.T) {
	closedFired := make(chan error, 1)
	sess := testSession(func(cause error) { closedFired <- cause })

	if !sess.endParked(ErrListenRevoked) {
		t.Fatal("the first end must report itself as first")
	}
	sess.flushParkedClose()

	select {
	case cause := <-closedFired:
		if !errors.Is(cause, ErrListenRevoked) {
			t.Fatalf("flushed close cause = %v, want ErrListenRevoked", cause)
		}
	default:
		t.Fatal("flushParkedClose never delivered the parked cause")
	}

	if err := sess.endCause(); !errors.Is(err, ErrListenRevoked) {
		t.Fatalf("endCause after a concurrent pump flushed the parked close = %v, want ErrListenRevoked — a death cause must outlive its one delivery to closedFn, or a page racing the flush reports a clean end for a revoked session", err)
	}
}

func TestSessionDeathCauseUpgradesFromYieldingLikeParkedClose(t *testing.T) {
	sess := testSession(func(error) {})

	if !sess.endYielding(errListenEnded) {
		t.Fatal("the first end must report itself as first")
	}
	sess.endParked(ErrListenRevoked)

	if err := sess.endCause(); !errors.Is(err, ErrListenRevoked) {
		t.Fatalf("endCause after a non-yielding cause displaced a yielding one = %v, want ErrListenRevoked", err)
	}
	sess.flushParkedClose()
	if err := sess.endCause(); !errors.Is(err, ErrListenRevoked) {
		t.Fatalf("endCause after the upgrade was flushed = %v, want ErrListenRevoked", err)
	}
}
