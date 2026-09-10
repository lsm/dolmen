package store

import (
	"testing"
	"time"
)

// Slice 6b (r6a): the live half's scaffolding. The observable contract
// today is its discipline: the wake is flag-only (it will run on
// committing writers' goroutines), and cancel quiesces a session with a
// parked pump — unregister, broadcast, wait — instead of blocking on it.

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
