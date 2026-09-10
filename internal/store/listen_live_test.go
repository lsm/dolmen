package store

import (
	"testing"
	"time"
)

// Slice 6b (r6a, r6b): the live half's scaffolding, then its registry
// wiring. The observable contracts: the wake is flag-only (it runs on
// committing writers' goroutines), cancel quiesces a session with a
// parked pump — unregister, broadcast, wait — instead of blocking on
// it, registration holds a registry entry for exactly the session's
// life, and a real commit dispatches into the wake.

// TestListenWakeIsFlagOnly pins BOTH halves of the wake contract: it runs
// on COMMITTING writers' goroutines, so it may only raise the flag and
// signal — a database call there would hold the writer's own transaction
// open (a wake holding the rw lock would deadlock against the very commit
// it reports) — and the flag without the signal is a lost wake: a pump
// that parked before it never learns a commit landed.
func TestListenWakeIsFlagOnly(t *testing.T) {
	sess := testSession(nil)

	parked := make(chan struct{})
	released := make(chan struct{})
	sess.pumps.Add(1)
	go func() {
		defer sess.pumps.Done()
		sess.mu.Lock()
		close(parked)
		for !sess.woken && !sess.dead {
			sess.cond.Wait()
		}
		sess.mu.Unlock()
		close(released)
	}()
	<-parked

	sess.wake("notes", ChangeRange{First: 1, Last: 1, Count: 1})

	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("wake raised the flag but never released the parked pump — a lost wake")
	}
	sess.mu.Lock()
	woken := sess.woken
	sess.mu.Unlock()
	if !woken {
		t.Fatal("wake left the flag unset")
	}
	sess.cancel() // idempotent teardown; also waits the pump goroutine out
}

// TestSessionCancelWakesWaitingPump: an idle pump is a goroutine parked in
// cond.Wait — the normal case, no commit ever arrived — and cancel must
// reach it through end's broadcast and return only after it has exited:
// unregister first (no new wakes), broadcast before the wait (r5's review
// found exactly the blocking shape this pins out).
func TestSessionCancelWakesWaitingPump(t *testing.T) {
	sess := testSession(nil)
	sess.pumps.Add(1)
	go func() {
		defer sess.pumps.Done()
		sess.mu.Lock()
		for !sess.dead {
			sess.cond.Wait()
		}
		sess.mu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		sess.cancel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel blocked on an idle pump — end() must broadcast before pumps.Wait")
	}
	sess.cancel() // idempotent
}

// TestListenJoinsCommitRegistry: a live session holds a registry entry
// from registration to cancel — joined before the boundary transaction
// (the register-and-replay ordering's home), removed at teardown,
// nothing leaked either way.
func TestListenJoinsCommitRegistry(t *testing.T) {
	st := openChangeStore(t)
	regCount := func() int {
		st.notifyMu.Lock()
		defer st.notifyMu.Unlock()
		return len(st.listeners["test"])
	}
	if got := regCount(); got != 0 {
		t.Fatalf("baseline registry held %d listeners, want 0", got)
	}
	_, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, nil)
	if got := regCount(); got != 1 {
		t.Fatalf("registered session held %d listeners, want 1", got)
	}
	cancel()
	if got := regCount(); got != 0 {
		t.Fatalf("cancelled session left %d listeners behind, want 0", got)
	}
}

// TestListenCommitRaisesWakeFlag: the registry dispatch reaches the wake
// through the REAL write path — notifyCommitted runs on the writing
// goroutine, so by the time the write call returns, the flag is up. The
// flag is the work queue; the durable log the record of truth.
func TestListenCommitRaisesWakeFlag(t *testing.T) {
	st := openChangeStore(t)
	sess := testSession(nil)
	sess.unregister = st.onCommit("test", sess.wake)
	defer sess.cancel()

	insertNotes(t, st, 1)

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if !sess.woken {
		t.Fatal("a committed write left the wake flag unset — dispatch is not wired to the session")
	}
}
