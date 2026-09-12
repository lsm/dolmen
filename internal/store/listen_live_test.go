package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
	sess.cancel()
}

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
	sess.cancel()
}

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

func TestListenBoundaryHoldsAgainstCommits(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 3)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()
	replayed := drainReplay(t, replay)
	if got := rowIDsOf(replayed); len(got) != 3 || got[0] != backlog.Ids[0] || got[2] != backlog.Ids[2] {
		t.Fatalf("replay delivered rows %v, want the 3-record backlog %v", got, backlog.Ids)
	}

	insertNotes(t, st, 5)
	records, _, done, err := replay.Next(context.Background())
	if err != nil {
		t.Fatalf("post-drain Next: %v", err)
	}
	if len(records) != 0 || !done {
		t.Fatalf("post-drain Next = %d records, done=%v, want 0, true — the boundary held", len(records), done)
	}
}

func TestListenCancelReleasesPump(t *testing.T) {
	st := openChangeStore(t)

	_, cancel := listenOn(t, st, "", "", func(ChangeRecord) {}, nil)

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
	cancel()
}

func TestListenCancelUnblocksParkedFill(t *testing.T) {
	st := openChangeStore(t)
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	hold, err := n.rw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("hold rw: %v", err)
	}
	defer hold.Rollback()

	sess := testSession(nil)
	sess.n = n
	sess.pumps.Add(1)
	go sess.pump()
	sess.wake("notes", ChangeRange{First: 1, Last: 1, Count: 1})

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

func TestListenFillQueuesCommits(t *testing.T) {
	st := openChangeStore(t)
	backlog := insertNotes(t, st, 3)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n
	sess.flight = make(chan struct{}, 1)
	sess.liveRead = int64(len(backlog.Ids))
	sess.chain = newCursorChain(time.Now(), sess.liveRead)
	sess.unregister = st.onCommit("test", sess.wake)
	sess.pumps.Add(1)
	go sess.pump()
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

func TestListenOverflowTeachingClose(t *testing.T) {
	st := openChangeStore(t)
	closedCause := make(chan error, 1)

	_, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, func(cause error) { closedCause <- cause })
	defer cancel()

	if _, err := insertNotesChunkedErr(st, listenQueueBound+MaxChangesPageLimit); err != nil {
		t.Fatalf("bulk write: %v", err)
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenOverflow) {
			t.Fatalf("overflow close cause = %v, want ErrListenOverflow", cause)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no overflow close: the bounded queue never tripped")
	}
}

func TestListenOverflowCloseToleratesCancelFromCallback(t *testing.T) {
	st := openChangeStore(t)
	cancelled := make(chan struct{})

	var cancel func()
	_, cancel = listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, func(cause error) {
		cancel()
		close(cancelled)
	})
	defer cancel()

	if _, err := insertNotesChunkedErr(st, listenQueueBound+MaxChangesPageLimit); err != nil {
		t.Fatalf("bulk write: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(20 * time.Second):
		t.Fatal("overflow close deadlocked: cancel from closedFn waited on the pump it ran on")
	}
}

func TestListenCancelDuringFiringCallback(t *testing.T) {
	st := openChangeStore(t)
	firing := make(chan struct{})
	reentered := make(chan struct{})
	release := make(chan struct{})

	var cancel func()
	_, cancel = listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, func(cause error) {
		close(firing)
		cancel()
		close(reentered)
		<-release
	})
	defer cancel()

	if _, err := insertNotesChunkedErr(st, listenQueueBound+MaxChangesPageLimit); err != nil {
		t.Fatalf("bulk write: %v", err)
	}
	select {
	case <-firing:
	case <-time.After(20 * time.Second):
		t.Fatal("no overflow close: the bounded queue never tripped")
	}

	done := make(chan struct{})
	go func() {
		cancel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("external cancel blocked on a firing terminal callback — the in-flight rule lets it return")
	}
	select {
	case <-reentered:
	case <-time.After(10 * time.Second):
		t.Fatal("reentrant cancel blocked inside the parked callback — the once body must hold no join")
	}
	close(release)
	cancel()
}

func insertNotesChunkedErr(st *Store, n int) (InsertResult, error) {
	ctx := context.Background()
	var out InsertResult
	for n > 0 {
		size := n
		if size > MaxRecordsPerInsert {
			size = MaxRecordsPerInsert
		}
		recs := make([]map[string]any, size)
		for i := range recs {
			recs[i] = map[string]any{"title": "a", "score": i + 1}
		}
		res, err := st.Insert(ctx, "test", "notes", recs, WriteOpts{}, Embedder{}, nil, Incarnation{})
		if err != nil {
			return out, err
		}
		out.Ids = append(out.Ids, res.Ids...)
		out.Changes.Count += res.Changes.Count
		n -= size
	}
	return out, nil
}
