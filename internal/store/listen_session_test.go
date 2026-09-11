package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// Slice 6b (r1): the session record — the lifecycle primitives every later
// 6b slice builds on. The fixtures construct the session directly (the
// registration transaction that creates it lands with the next slice) and
// pin the contract's teardown rules: closed never fires for a caller
// cancel, fires once for an engine end, survives a panicking callback, and
// cancel is idempotent.

// testSession is a minimal session for the lifecycle pins: only the fields
// these tests read. The cond is wired the way registration wires it —
// end's broadcast dereferences it, so a session without one would panic
// on its first teardown.
func testSession(closed func(error)) *listenSession {
	sess := &listenSession{nsName: "test", table: "notes", closedFn: closed}
	sess.cond = sync.NewCond(&sess.mu)
	sess.ctx, sess.ctxCancel = context.WithCancel(context.Background())
	return sess
}

// TestSessionCancelNeverCloses: cancel is the caller's teardown — closed
// never fires for it (§6.2) — and cancel is idempotent.
func TestSessionCancelNeverCloses(t *testing.T) {
	closedFired := make(chan error, 1)
	sess := testSession(func(cause error) { closedFired <- cause })

	sess.cancel()
	sess.cancel() // idempotent

	select {
	case cause := <-closedFired:
		t.Fatalf("closed fired on caller cancel with %v", cause)
	default:
	}
	if !sess.dead {
		t.Fatal("cancel left the session live")
	}
}

// TestSessionEndFiresClosedOnce: an engine-initiated end (a non-nil cause)
// fires closed exactly once with that cause, no matter how many times end
// runs; a later cancel adds nothing.
func TestSessionEndFiresClosedOnce(t *testing.T) {
	causes := make(chan error, 4)
	sess := testSession(func(cause error) { causes <- cause })

	boom := errors.New("engine end")
	sess.end(boom)
	sess.end(errors.New("second")) // absorbed: the first end wins
	sess.cancel()                  // the caller's cancel adds nothing

	if n := len(causes); n != 1 {
		t.Fatalf("closed fired %d times, want exactly once", n)
	}
	if got := <-causes; !errors.Is(got, boom) {
		t.Fatalf("close cause = %v, want %v", got, boom)
	}
}

// TestSessionClosedPanicRecovered: the terminal callback is caller code —
// a panicking close must not take the process down (deliverCommit's
// write-path rule, applied to the session). The direct end (a
// never-launched session) fires inline, so the callback provably RAN.
func TestSessionClosedPanicRecovered(t *testing.T) {
	ran := false
	sess := testSession(func(error) { ran = true; panic("boom") })
	sess.end(errors.New("cause"))
	// Reaching here at all is the pin: fireClosed recovered — and the
	// callback actually fired on the direct-end path.
	if !sess.dead {
		t.Fatal("session not dead after the panicking close")
	}
	if !ran {
		t.Fatal("the direct end never fired closedFn — the parked cause had no flusher")
	}
}

// TestSessionCursorIsStanding: cursor reports the standing resume cursor
// under the session lock, whatever the caller's goroutine last stored.
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
