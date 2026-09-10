package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Slice 6b (r6a–r6c): the live half's scaffolding, its registry wiring,
// then its producer. Commits wake the fill pump and the pump pages the
// durable log into the session's queue; the drain that delivers the
// queue and the bound that caps it land in the next slice. The pinned
// contracts: the wake is flag-only, registration holds a registry entry
// for exactly the session's life, the boundary holds against interim
// commits, cancel releases an idle pump, and commits land in the queue
// through the real write path.

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

// TestListenBoundaryHoldsAgainstCommits: a commit landing while the replay
// is still draining is absorbed by the queue (the producer half's exactly-
// once contribution): the replay still delivers exactly the backlog, the
// boundary R fixed at registration — nothing the interim commit added leaks
// into it (§6.2).
func TestListenBoundaryHoldsAgainstCommits(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 3)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()
	replayed := drainReplay(t, replay)
	if got := rowIDsOf(replayed); len(got) != 3 || got[0] != backlog.Ids[0] || got[2] != backlog.Ids[2] {
		t.Fatalf("replay delivered rows %v, want the 3-record backlog %v", got, backlog.Ids)
	}

	// Commits after the drained boundary land in the queue, not the
	// replay: the replay's range was fixed at registration, and a further
	// Next reports the (unchanged) boundary.
	insertNotes(t, st, 5)
	records, _, done, err := replay.Next(context.Background())
	if err != nil {
		t.Fatalf("post-drain Next: %v", err)
	}
	if len(records) != 0 || !done {
		t.Fatalf("post-drain Next = %d records, done=%v, want 0, true — the boundary held", len(records), done)
	}
}

// TestListenCancelReleasesPump: cancel wakes an IDLE pump through the real
// Listen path (the normal case — no commit ever arrived), waits it out,
// and returns: the use-after-free rule from notify.go's listener contract,
// and the exact shape of r5's review finding (end must broadcast before
// the wait).
func TestListenCancelReleasesPump(t *testing.T) {
	st := openChangeStore(t)

	_, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, nil)
	// No commit, no wake: the pump sleeps in cond.Wait. cancel must wake
	// it, not block on it.
	done := make(chan struct{})
	go func() {
		cancel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel blocked on an idle pump — end() must broadcast before pumps.Wait")
	}
	cancel() // idempotent
}

// TestListenCancelUnblocksParkedFill: the pump's reads run on the
// session's own scope, not Background — a fill waiting in BeginTx behind
// the namespace's single write connection must be released by cancel
// (end cancels the scope BEFORE pumps.Wait), or a caller tearing down
// mid-read blocks on it indefinitely.
func TestListenCancelUnblocksParkedFill(t *testing.T) {
	st := openChangeStore(t)
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	hold, err := n.rw.BeginTx(context.Background(), nil) // the single write connection, held
	if err != nil {
		t.Fatalf("hold rw: %v", err)
	}
	defer hold.Rollback()

	sess := testSession(nil)
	sess.n = n
	sess.pumps.Add(1)
	go sess.fill()
	sess.wake("notes", ChangeRange{First: 1, Last: 1, Count: 1}) // the pump takes the flag and parks in BeginTx

	done := make(chan struct{})
	go func() {
		sess.cancel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel blocked on a fill parked in BeginTx — end must cancel the session scope before pumps.Wait")
	}
}

// TestListenFillQueuesCommits pins the producer directly through the
// session's queue: commits after registration land there through the real
// write path — the wake, the fill's page of the durable log, and the
// append — which is what a later drain delivers from.
func TestListenFillQueuesCommits(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 3)

	sess := &listenSession{
		s: st, nsName: "test",
		flight: make(chan struct{}, 1),
	}
	sess.cond = sync.NewCond(&sess.mu)
	sess.ctx, sess.ctxCancel = context.WithCancel(context.Background())
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess.n = n
	sess.liveRead = int64(len(backlog.Ids)) // past the backlog
	sess.unregister = st.onCommit("test", sess.wake)
	sess.pumps.Add(1)
	go sess.fill()
	defer sess.cancel()

	insertNotes(t, st, 2)
	deadline := time.Now().Add(10 * time.Second)
	for {
		sess.mu.Lock()
		got := len(sess.queue)
		sess.mu.Unlock()
		if got == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue held %d records, want the 2 committed", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
