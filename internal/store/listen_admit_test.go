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

func TestListenAdmitDecisions(t *testing.T) {
	ownGen := [16]byte{1}
	foreignGen := [16]byte{2}
	rec := ChangeRecord{Table: "notes", Owner: "alice", Lifetime: Lifetime{NsGen: ownGen, Table: "notes"}}
	authzOf := func(scope *RowScope, inc Incarnation, ok bool) func(string) (*RowScope, Incarnation, bool) {
		return func(string) (*RowScope, Incarnation, bool) { return scope, inc, ok }
	}
	cases := []struct {
		name     string
		nsFeed   bool
		resolver func(string) (*RowScope, Incarnation, bool)
		record   ChangeRecord
		visible  bool
		revoked  bool
	}{
		{"granted-unscoped", false, authzOf(nil, Incarnation{}, true), rec, true, false},
		{"own-scope", false, authzOf(&RowScope{Owner: "alice"}, Incarnation{}, true), rec, true, false},
		{"foreign-scope", false, authzOf(&RowScope{Owner: "bob"}, Incarnation{}, true), rec, false, false},
		{"empty-scope", false, authzOf(&RowScope{Empty: true}, Incarnation{}, true), rec, false, false},
		{"revoked", false, authzOf(nil, Incarnation{}, false), rec, false, true},
		{"own-nsGen-nsFeed", true, authzOf(nil, Incarnation{NsGen: ownGen}, true), rec, true, false},
		{"foreign-nsGen-nsFeed", true, authzOf(nil, Incarnation{NsGen: foreignGen}, true), rec, false, false},
		{"own-lifetime-tableFeed", false, authzOf(nil, Incarnation{NsGen: ownGen, Table: "notes"}, true), rec, true, false},
		{"foreign-nsGen-tableFeed", false, authzOf(nil, Incarnation{NsGen: foreignGen, Table: "notes"}, true), rec, false, false},
		{"partial-inc-tableFeed-hides", false, authzOf(nil, Incarnation{NsGen: ownGen}, true), rec, false, false},
		{"zero-inc-nsFeed-admits", true, authzOf(nil, Incarnation{}, true), rec, true, false},
	}
	for _, tc := range cases {
		sess := testSession(nil)
		if !tc.nsFeed {
			sess.table = "notes"
		} else {
			sess.table = ""
		}
		sess.liveAuthz = tc.resolver
		visible, revoked := sess.admit(tc.record)
		if visible != tc.visible || revoked != tc.revoked {
			t.Fatalf("admit(%s) = visible %v, revoked %v; want %v, %v", tc.name, visible, revoked, tc.visible, tc.revoked)
		}
	}

	ownLifetime := ChangeRecord{Table: "notes", Lifetime: Lifetime{NsGen: ownGen, Table: "notes", DropGen: 3}}
	incMatch := Incarnation{NsGen: ownGen, Table: "notes", DropGen: 3}
	incDrop := Incarnation{NsGen: ownGen, Table: "notes", DropGen: 4}
	incTable := Incarnation{NsGen: ownGen, Table: "other", DropGen: 3}
	tableSess := func(inc Incarnation) *listenSession {
		sess := testSession(nil)
		sess.table = "notes"
		sess.liveAuthz = authzOf(nil, inc, true)
		return sess
	}
	if visible, _ := tableSess(incMatch).admit(ownLifetime); !visible {
		t.Fatal("a matching incarnation must admit on a table feed")
	}
	if visible, _ := tableSess(incDrop).admit(ownLifetime); visible {
		t.Fatal("a drop-generation mismatch must hide on a table feed")
	}
	if visible, _ := tableSess(incTable).admit(ownLifetime); visible {
		t.Fatal("a table mismatch must hide on a table feed")
	}
	nsSess := tableSess(incTable)
	nsSess.table = ""
	if visible, _ := nsSess.admit(ownLifetime); !visible {
		t.Fatal("a namespace feed spans table lifetimes — a table mismatch alone must admit")
	}

	nilSess := testSession(nil)
	if visible, revoked := nilSess.admit(rec); !visible || revoked {
		t.Fatalf("nil liveAuthz = visible %v, revoked %v; want true, false", visible, revoked)
	}
}

func TestListenLiveAdmissionFiltersForeignRecordsAndConsumesInvisible(t *testing.T) {
	st := openChangeStore(t)

	alice, err := st.Insert(context.Background(), "test", "notes", []map[string]any{{"title": "a", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	bob, err := st.Insert(context.Background(), "test", "notes", []map[string]any{{"title": "b", "score": 2}, {"title": "c", "score": 3}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	labelNotesOwners(t, st, "alice", alice.Ids)
	labelNotesOwners(t, st, "bob", bob.Ids)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	delivered := make(chan int64, 2)
	sess := testSession(nil)
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)
	sess.liveAuthz = scopedAuthz("alice")
	sess.notify = func(r ChangeRecord) { delivered <- r.RowID }
	sess.replayDone = true

	read, err := sess.fillBatch()
	if err != nil {
		t.Fatalf("fillBatch: %v", err)
	}
	if read != 3 {
		t.Fatalf("fillBatch read %d, want all 3 scanned positions consumed", read)
	}
	sess.mu.Lock()
	queued := len(sess.queue)
	liveRead := sess.liveRead
	sess.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queue held %d records, want only alice's 1 — foreign records crossed the gate", queued)
	}
	if liveRead != 3 {
		t.Fatalf("liveRead = %d, want 3 — invisible positions must be consumed exactly once", liveRead)
	}

	sess.pumps.Add(1)
	go sess.drain()
	select {
	case id := <-delivered:
		if id != alice.Ids[0] {
			t.Fatalf("delivered row %d, want alice's row %d", id, alice.Ids[0])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the admitted record never delivered")
	}
	select {
	case id := <-delivered:
		t.Fatalf("foreign record row %d delivered — the gate must hide it", id)
	case <-time.After(200 * time.Millisecond):
	}
	sess.cancel()
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
