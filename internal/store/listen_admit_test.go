package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func labelNotesOwners(t *testing.T, st *Store, owner string, ids []int64) {
	t.Helper()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	tx, err := n.rw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("labels tx: %v", err)
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(context.Background(),
			`UPDATE _dolmen_changes SET owner = ? WHERE table_name = 'notes' AND row_id = ?`, owner, id); err != nil {
			t.Fatalf("label row %d: %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("labels commit: %v", err)
	}
}

func scopedAuthz(owner string) func(table string) (*RowScope, Incarnation, bool) {
	return func(table string) (*RowScope, Incarnation, bool) {
		return &RowScope{Owner: owner}, Incarnation{}, true
	}
}

func TestListenLiveAdmissionFiltersForeignRecords(t *testing.T) {
	st := openChangeStore(t)

	got := make(chan ChangeRecord, 4)
	replay, cancel, err := st.Listen(context.Background(), "test", "", "", [16]byte{}, scopedAuthz("alice"), func(r ChangeRecord) { got <- r }, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}

	alice, err := st.Insert(context.Background(), "test", "notes", []map[string]any{{"title": "a", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert alice: %v", err)
	}
	bob, err := st.Insert(context.Background(), "test", "notes", []map[string]any{{"title": "b", "score": 2}, {"title": "c", "score": 3}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert bob: %v", err)
	}
	alice2, err := st.Insert(context.Background(), "test", "notes", []map[string]any{{"title": "d", "score": 4}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert alice2: %v", err)
	}
	labelNotesOwners(t, st, "alice", []int64{alice.Ids[0], alice2.Ids[0]})
	labelNotesOwners(t, st, "bob", bob.Ids)

	seen := 0
	want := []int64{alice.Ids[0], alice2.Ids[0]}
	for seen < len(want) {
		select {
		case r := <-got:
			if r.RowID != want[seen] {
				t.Fatalf("live record row %d delivered, want %d — a foreign record crossed the gate", r.RowID, want[seen])
			}
			seen++
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d own records delivered", seen, len(want))
		}
	}
	select {
	case r := <-got:
		t.Fatalf("extra delivery row %d after the subscriber's own records — foreign traffic crossed the gate", r.RowID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestListenLiveAdmissionEmptyScopeDeliversNothing(t *testing.T) {
	st := openChangeStore(t)

	got := make(chan ChangeRecord, 2)
	replay, cancel, err := st.Listen(context.Background(), "test", "", "", [16]byte{}, func(table string) (*RowScope, Incarnation, bool) {
		return &RowScope{Empty: true}, Incarnation{}, true
	}, func(r ChangeRecord) { got <- r }, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}

	insertNotes(t, st, 2)
	select {
	case r := <-got:
		t.Fatalf("record row %d delivered through an empty scope — the gate must hide everything", r.RowID)
	case <-time.After(300 * time.Millisecond):
	}
	if replay.Resume() == "" {
		t.Fatal("the session lost its standing cursor while idling behind an empty scope")
	}
}

func TestListenLiveAdmissionIncarnationMismatchHidesRecords(t *testing.T) {
	st := openChangeStore(t)

	got := make(chan ChangeRecord, 2)
	replay, cancel, err := st.Listen(context.Background(), "test", "notes", "", [16]byte{}, func(table string) (*RowScope, Incarnation, bool) {
		return nil, Incarnation{NsGen: [16]byte{0xff}, Table: table}, true
	}, func(r ChangeRecord) { got <- r }, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}

	insertNotes(t, st, 2)
	select {
	case r := <-got:
		t.Fatalf("record row %d delivered across a mismatched incarnation — a stale decision crossed onto foreign records", r.RowID)
	case <-time.After(300 * time.Millisecond):
	}
	if replay.Resume() == "" {
		t.Fatal("the session lost its standing cursor while idling behind a mismatched incarnation")
	}
}

func TestListenLiveRevocationDeliversPrefixThenCloses(t *testing.T) {
	st := openChangeStore(t)

	delivered := make(chan int64, 2)
	closedCause := make(chan error, 1)
	var allow atomic.Bool
	allow.Store(true)
	authz := func(table string) (*RowScope, Incarnation, bool) {
		if !allow.Load() {
			return nil, Incarnation{}, false
		}
		return nil, Incarnation{}, true
	}
	replay, cancel, err := st.Listen(context.Background(), "test", "", "", [16]byte{}, authz, func(r ChangeRecord) { delivered <- r.RowID }, func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}

	insertNotes(t, st, 1)
	select {
	case id := <-delivered:
		if id != 1 {
			t.Fatalf("prefix delivered row %d, want row 1 (the pre-revocation record)", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pre-revocation record never delivered — the gate blocked a granted read")
	}
	allow.Store(false)
	insertNotes(t, st, 1)

	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenRevoked) {
			t.Fatalf("revocation close cause = %v, want ErrListenRevoked", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("revocation never closed the session")
	}
	select {
	case id := <-delivered:
		t.Fatalf("record row %d delivered after the revocation — the close must precede everything after it", id)
	default:
	}
}

func TestListenLiveRevocationMidBatchDeliversAdmittedPrefix(t *testing.T) {
	st := openChangeStore(t)

	delivered := make(chan int64, 2)
	closedCause := make(chan error, 1)
	var calls atomic.Int64
	authz := func(table string) (*RowScope, Incarnation, bool) {
		if calls.Add(1) >= 2 {
			return nil, Incarnation{}, false
		}
		return nil, Incarnation{}, true
	}
	replay, cancel, err := st.Listen(context.Background(), "test", "", "", [16]byte{}, authz, func(r ChangeRecord) { delivered <- r.RowID }, func(cause error) { closedCause <- cause })
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cancel()
	if _, _, done, err := replay.Next(context.Background()); err != nil || !done {
		t.Fatalf("boundary Next = err %v, done %v, want nil error, done=true", err, done)
	}

	insertNotes(t, st, 2)
	select {
	case id := <-delivered:
		if id != 1 {
			t.Fatalf("prefix delivered row %d, want row 1 (the batch's admitted head)", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the admitted prefix never delivered before the mid-batch revocation close")
	}
	select {
	case cause := <-closedCause:
		if !errors.Is(cause, ErrListenRevoked) {
			t.Fatalf("mid-batch revocation close cause = %v, want ErrListenRevoked", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mid-batch revocation never closed the session")
	}
	select {
	case id := <-delivered:
		t.Fatalf("record row %d delivered after the mid-batch revocation — row 2's position was consumed, never delivered", id)
	default:
	}
}
