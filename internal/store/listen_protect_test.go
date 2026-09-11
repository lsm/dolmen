package store

import (
	"context"
	"testing"
	"time"
)

func protectedSession(t *testing.T, st *Store) *listenSession {
	t.Helper()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n
	return sess
}

func tokenOriginsAt(t *testing.T, st *Store, position int64) (origins []int64) {
	t.Helper()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	rows, err := n.ro.QueryContext(context.Background(),
		`SELECT chain_origin FROM _dolmen_cursor_tokens WHERE position = ?`, position)
	if err != nil {
		t.Fatalf("query protective tokens: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var origin int64
		if err := rows.Scan(&origin); err != nil {
			t.Fatalf("scan origin: %v", err)
		}
		origins = append(origins, origin)
	}
	return origins
}

func TestListenProtectQueueNoopInsideMargin(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.chain = newCursorChain(time.Now(), 0)
	sess.queue = []loggedChange{{seq: 10, rec: ChangeRecord{RowID: 10}}, {seq: 11, rec: ChangeRecord{RowID: 11}}}
	sess.liveRead = 11
	sess.replayExhausted = true

	sess.protectQueue(false)

	if got := len(tokenOriginsAt(t, st, 10)); got != 0 {
		t.Fatalf("protectQueue minted %d tokens on a comfortably-inside chain, want 0", got)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.chain.Start < time.Now().Add(-time.Minute).UnixMilli() {
		t.Fatal("protectQueue rotated a chain comfortably inside its cap")
	}
}

func TestListenProtectQueueRotatesNearCap(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.chain = newCursorChain(time.Now().Add(-st.changeRetention+100*time.Millisecond), 0)
	sess.queue = []loggedChange{{seq: 10, rec: ChangeRecord{RowID: 10}}, {seq: 11, rec: ChangeRecord{RowID: 11}}}
	sess.liveRead = 11
	sess.replayExhausted = true
	sess.position = 2

	sess.protectQueue(false)

	origins := tokenOriginsAt(t, st, 10)
	if len(origins) != 1 || origins[0] != 9 {
		t.Fatalf("protective token origins = %v, want exactly [9] (strictly below the queue head)", origins)
	}
	sess.mu.Lock()
	rotated := sess.chain
	sess.mu.Unlock()
	if rotated == nil || rotated.Start < time.Now().Add(-time.Minute).UnixMilli() {
		t.Fatal("protectQueue left the near-cap chain current")
	}
}

func TestListenProtectQueueEmptyQueueSkips(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.chain = newCursorChain(time.Now().Add(-st.changeRetention), 0)
	sess.liveRead = 5
	before := sess.chain

	sess.protectQueue(false)

	if sess.chain != before {
		t.Fatal("protectQueue rotated with nothing queued to protect")
	}
	if got := len(tokenOriginsAt(t, st, 5)); got != 0 {
		t.Fatalf("empty-queue protectQueue minted %d tokens, want 0", got)
	}
}

func TestListenProtectQueueRetentionZeroSkips(t *testing.T) {
	st, err := Open(t.TempDir(), WithChangeRetention(0))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateNamespace(context.Background(), "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	sess := protectedSession(t, st)
	sess.chain = newCursorChain(time.Now(), 0)
	sess.queue = []loggedChange{{seq: 3, rec: ChangeRecord{RowID: 3}}}
	sess.liveRead = 3

	sess.protectQueue(false)

	if got := len(tokenOriginsAt(t, st, 3)); got != 0 {
		t.Fatalf("retention-0 store minted %d protective tokens, want 0", got)
	}
}

func TestListenProtectQueueSurvivesPrune(t *testing.T) {
	st := openStampedStore(t)
	old := time.Now().Add(-200 * time.Millisecond)
	seedStampedChanges(t, st, "notes", []time.Time{old, old, old})
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	ctx := context.Background()

	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := pruneChanges(ctx, tx, time.Now(), st.changeRetention); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := len(changeSeqs(t, n)); got != 0 {
		t.Fatalf("unprotected aged rows survived prune: %v", got)
	}

	seedStampedChanges(t, st, "notes", []time.Time{old, old, old})
	sess := protectedSession(t, st)
	sess.chain = newCursorChain(time.Now(), 0)
	sess.queue = []loggedChange{{seq: 5, rec: ChangeRecord{RowID: 2}}, {seq: 6, rec: ChangeRecord{RowID: 3}}}
	sess.liveRead = 6
	sess.replayExhausted = true
	sess.protectQueue(false)

	tx, err = n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := pruneChanges(ctx, tx, time.Now(), st.changeRetention); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if seqs := changeSeqs(t, n); len(seqs) != 2 || seqs[0] != 5 || seqs[1] != 6 {
		t.Fatalf("protected queue rows = %v, want [5 6]", seqs)
	}
}

func TestListenChainForFloorsAtQueueHead(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.chain = &cursorChain{ID: "old", Origin: 0, Start: time.Now().Add(-2 * st.changeRetention).UnixMilli()}
	sess.queue = []loggedChange{{seq: 40, rec: ChangeRecord{RowID: 40}}}
	sess.replayExhausted = true

	chain := sess.chainFor(time.Now(), 100)

	if chain.Origin != 40 {
		t.Fatalf("rotated chain rooted at %d, want the queue head 40", chain.Origin)
	}
}

func TestListenProtectQueueForceMintsInsideMargin(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.chain = newCursorChain(time.Now(), 0)
	sess.queue = []loggedChange{{seq: 7, rec: ChangeRecord{RowID: 7}}}
	sess.liveRead = 7
	sess.replayExhausted = true

	sess.protectQueue(true)

	origins := tokenOriginsAt(t, st, 7)
	if len(origins) != 1 || origins[0] != 6 {
		t.Fatalf("forced protection origins = %v, want exactly [6] inside a fresh chain's margin", origins)
	}
}
