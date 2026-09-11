package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// The strengthened replay terminal contract: Next never reports a dead
// session as a clean done — every death returns its cause as the replay
// error, so clean done means provably alive. The two pins below are the
// in-page deaths the handler cannot see coming; the drop's
// evict-before-end ordering is pinned at the conformance layer, where the
// teaching framing is the observable.

func TestListenReplayRevocationReturnsCauseAsError(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	closedCause := make(chan error, 1)
	var allow atomic.Bool
	allow.Store(true)
	authz := func(table string) (*RowScope, Incarnation, bool) {
		if !allow.Load() {
			return nil, Incarnation{}, false
		}
		return nil, Incarnation{}, true
	}
	replay, cancel, err := st.Listen(context.Background(), "test", "", "begin", [16]byte{}, authz,
		func(ChangeRecord) {}, func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()

	allow.Store(false)
	_, _, done, nerr := replay.Next(context.Background())
	if nerr == nil {
		t.Fatalf("revoked replay Next = clean done %v, want the cause as the error — clean done must mean provably alive", done)
	}
	if !errors.Is(nerr, ErrListenRevoked) {
		t.Fatalf("revoked replay Next error = %v, want ErrListenRevoked", nerr)
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenRevoked) {
			t.Fatalf("revocation closed cause = %v, want ErrListenRevoked", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("revocation never fired the closed callback")
	}
}

func TestListenReplayLossReturnsTeachingAsError(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 3)

	closedCause := make(chan error, 1)
	replay, cancel, err := st.Listen(context.Background(), "test", "", "begin", [16]byte{}, nil,
		func(ChangeRecord) {}, func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	if _, err := n.rw.ExecContext(context.Background(),
		`DELETE FROM _dolmen_changes WHERE row_id = 1 AND table_name = 'notes'`); err != nil {
		t.Fatalf("delete behind the guarded prune: %v", err)
	}

	_, _, done, nerr := replay.Next(context.Background())
	if nerr == nil {
		t.Fatalf("lossy replay Next = clean done %v, want the teaching as the error — clean done must mean provably alive", done)
	}
	if !errors.Is(nerr, ErrCursorExpired) {
		t.Fatalf("lossy replay Next error = %v, want the cursor-expired teaching", nerr)
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrCursorExpired) {
			t.Fatalf("loss closed cause = %v, want the cursor-expired teaching", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the loss never fired the closed callback")
	}
}

// The symptom-yields-to-verdict park rule, pinned at the session level: a
// page's incidental error parks first (evict closed the pools behind a
// drop whose endListenSessions has not run), and the drop's later
// authoritative end must displace it — the subscriber is taught the
// lifetime end, never the database noise. And once the flush has taken a
// cause (the slot is empty), a later end parks nothing: no pump remains
// to fire a fresh park.
func TestListenSymptomParkYieldsToVerdict(t *testing.T) {
	sess := testSession(nil)

	symptom := errors.New("sql: database is closed")
	if !sess.endYielding(symptom) {
		t.Fatal("the first end did not arm the session")
	}
	if got := sess.endCause(); !errors.Is(got, symptom) {
		t.Fatalf("parked cause = %v, want the symptom", got)
	}
	if sess.endParked(ErrListenLifetimeEnded) {
		t.Fatal("a second end reported itself first")
	}
	if got := sess.endCause(); !errors.Is(got, ErrListenLifetimeEnded) {
		t.Fatalf("parked cause after the verdict = %v, want ErrListenLifetimeEnded — the symptom must yield", got)
	}

	fired := testSession(nil)
	if !fired.endParked(ErrListenOverflow) {
		t.Fatal("the first end did not arm the session")
	}
	fired.mu.Lock()
	fired.pendingClose = nil // the flush took it: the cause has fired
	fired.mu.Unlock()
	if fired.endParked(ErrListenLifetimeEnded) {
		t.Fatal("a second end reported itself first")
	}
	fired.mu.Lock()
	late := fired.pendingClose
	fired.pendingCloseYield = false
	fired.mu.Unlock()
	if late != nil {
		t.Fatalf("a late end parked %v into an empty slot — no pump remains to fire it", late)
	}
}

// The deferral barrier, pinned with the store mutex held the way a drop
// holds it: a yielded symptom must not fire while the mutex is held, and
// once the authoritative verdict parks under that same mutex, the fire
// after release carries the verdict — never the incidental read error.
// A per-call cancellation is the caller's, not the session's: a Next whose
// own deadline expires mid-page returns the context error and leaves the
// session — and its standing cursor — retryable, exactly as a cancellation
// before the flight permit always did.
func TestListenCanceledPageLeavesSessionRetryable(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	replay, cancel := listenOn(t, st, "", CursorBegin, func(ChangeRecord) {}, nil)
	defer cancel()

	canceled, cancelCall := context.WithCancel(context.Background())
	cancelCall()
	if _, _, _, err := replay.Next(canceled); err == nil {
		t.Fatal("a canceled Next reported success")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Next error = %v, want the context error", err)
	}

	records, _, done, err := replay.Next(context.Background())
	if err != nil {
		t.Fatalf("the retry after a canceled page failed: %v — per-call cancellation must not end the session", err)
	}
	if len(records) != 2 || done {
		t.Fatalf("retried page = %d records, done=%v, want the full backlog with the page still open — done belongs to the empty boundary call", len(records), done)
	}
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("the boundary call after the retry = err %v, done %v, want the empty boundary done", err, done)
	}
}

func TestListenYieldedFlushDefersToTheVerdict(t *testing.T) {
	st := openChangeStore(t)
	fired := make(chan error, 1)
	sess := testSession(func(cause error) { fired <- cause })
	sess.s = st

	symptom := errors.New("sql: database is closed")
	if !sess.endYielding(symptom) {
		t.Fatal("the first end did not arm the session")
	}

	flushDone := make(chan struct{})
	st.mu.Lock()
	go func() {
		defer close(flushDone)
		sess.flushParkedClose()
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case cause := <-fired:
		t.Fatalf("a cause fired while the store mutex was held: %v — the symptom must defer behind a drop", cause)
	default:
	}
	if sess.endParked(ErrListenLifetimeEnded) {
		t.Fatal("a second end reported itself first")
	}
	st.mu.Unlock()

	select {
	case cause := <-fired:
		if !errors.Is(cause, ErrListenLifetimeEnded) {
			t.Fatalf("fired cause = %v, want the verdict — the symptom must have deferred behind the mutex", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the flush never fired after the mutex released")
	}
	<-flushDone
}
