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
	epoch := newCursorChain(time.Now(), 9)
	sess.queueChain = epoch
	sess.queue = []loggedChange{{seq: 10, rec: ChangeRecord{RowID: 10}}, {seq: 11, rec: ChangeRecord{RowID: 11}}}

	sess.protectQueue()

	if got := len(tokenOriginsAt(t, st, 10)); got != 0 {
		t.Fatalf("protectQueue minted %d tokens on a comfortably-inside epoch, want 0", got)
	}
	sess.mu.Lock()
	kept := sess.queueChain
	sess.mu.Unlock()
	if kept != epoch {
		t.Fatal("protectQueue replaced an epoch comfortably inside its cap")
	}
}

func TestListenProtectQueueRefreshesNearCap(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.queueChain = newCursorChain(time.Now().Add(-st.changeRetention+100*time.Millisecond), 9)
	sess.queue = []loggedChange{{seq: 10, rec: ChangeRecord{RowID: 10}}, {seq: 11, rec: ChangeRecord{RowID: 11}}}

	sess.protectQueue()

	origins := tokenOriginsAt(t, st, 10)
	if len(origins) != 1 || origins[0] != 9 {
		t.Fatalf("protective token origins = %v, want exactly [9] (strictly below the queue head)", origins)
	}
	sess.mu.Lock()
	refreshed := sess.queueChain
	sess.mu.Unlock()
	if refreshed == nil || refreshed.Start < time.Now().Add(-time.Minute).UnixMilli() {
		t.Fatal("protectQueue left a near-cap chain current")
	}
}

func TestListenProtectQueueRenewsOffTheTickGrid(t *testing.T) {
	st, err := Open(t.TempDir(), WithChangeRetention(501*time.Millisecond))
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
	sess.queueChain = newCursorChain(time.Now().Add(-listenPollInterval), 9)
	sess.queue = []loggedChange{{seq: 10, rec: ChangeRecord{RowID: 10}}}

	sess.protectQueue()

	origins := tokenOriginsAt(t, st, 10)
	if len(origins) != 1 || origins[0] != 9 {
		t.Fatalf("protective token origins = %v, want exactly [9]: the renewal is sampled a tick early, not a millisecond before expiry",
			origins)
	}
}

func TestListenProtectQueuePinsBelowTheQueueHeadNotTheReplayPosition(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.position = 1
	sess.liveRead = 41
	sess.replayExhausted = false
	sess.queue = []loggedChange{{seq: 40, rec: ChangeRecord{RowID: 40}}, {seq: 41, rec: ChangeRecord{RowID: 41}}}

	sess.protectQueue()

	origins := tokenOriginsAt(t, st, 40)
	if len(origins) != 1 || origins[0] != 39 {
		t.Fatalf("protective token origins = %v, want exactly [39]: a stalled replay must not drag the queue's root down to its position",
			origins)
	}
}

func TestListenProtectQueueMintsOnTheFirstObligation(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.queue = []loggedChange{{seq: 7, rec: ChangeRecord{RowID: 7}}}

	sess.protectQueue()

	origins := tokenOriginsAt(t, st, 7)
	if len(origins) != 1 || origins[0] != 6 {
		t.Fatalf("first-batch protection origins = %v, want exactly [6]", origins)
	}
}

func TestListenProtectQueueEmptyQueueSkips(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.queueChain = newCursorChain(time.Now().Add(-2*st.changeRetention), 4)
	sess.liveRead = 5

	sess.protectQueue()

	if got := len(tokenOriginsAt(t, st, 5)); got != 0 {
		t.Fatalf("empty-queue protectQueue minted %d tokens, want 0", got)
	}
}

func TestListenProtectQueueLeavesTheDeliveryChainAlone(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	delivery := newCursorChain(time.Now().Add(-st.changeRetention/2), 3)
	sess.chain = delivery
	sess.queue = []loggedChange{{seq: 5, rec: ChangeRecord{RowID: 5}}}

	sess.protectQueue()

	sess.mu.Lock()
	chain, epoch := sess.chain, sess.queueChain
	sess.mu.Unlock()
	if chain != delivery {
		t.Fatal("protectQueue replaced the delivery chain; its absolute cap must stay anchored")
	}
	if epoch == nil || epoch == delivery {
		t.Fatal("the queued prefix must ride a protective chain of its own")
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
	sess.queue = []loggedChange{{seq: 3, rec: ChangeRecord{RowID: 3}}}

	sess.protectQueue()

	if got := len(tokenOriginsAt(t, st, 3)); got != 0 {
		t.Fatalf("retention-0 store minted %d protective tokens, want 0", got)
	}
}

func TestListenProtectQueueNegativeRetentionSkips(t *testing.T) {
	st, err := Open(t.TempDir(), WithChangeRetention(-time.Hour))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess := testSession(nil)
	sess.s = st
	sess.queue = []loggedChange{{seq: 1, rec: ChangeRecord{RowID: 1}}}

	sess.protectQueue()

	sess.mu.Lock()
	chain := sess.queueChain
	sess.mu.Unlock()
	if chain != nil {
		t.Fatal("a disabling retention minted a protective token no prune would ever reclaim")
	}
}

func TestListenProtectQueueSubMillisecondRetentionSkips(t *testing.T) {
	st, err := Open(t.TempDir(), WithChangeRetention(time.Nanosecond))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess := testSession(nil)
	sess.s = st
	sess.queue = []loggedChange{{seq: 1, rec: ChangeRecord{RowID: 1}}}

	sess.protectQueue()

	sess.mu.Lock()
	epoch := sess.queueChain
	sess.mu.Unlock()
	if epoch != nil {
		t.Fatal("sub-millisecond retention admitted a protective epoch no refresh cadence can keep alive")
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
	sess.queue = []loggedChange{{seq: 5, rec: ChangeRecord{RowID: 2}}, {seq: 6, rec: ChangeRecord{RowID: 3}}}
	sess.protectQueue()

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

func TestListenChainForFloorsBelowQueueHead(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	sess.chain = &cursorChain{ID: "old", Origin: 0, Start: time.Now().Add(-2 * st.changeRetention).UnixMilli()}
	sess.queue = []loggedChange{{seq: 40, rec: ChangeRecord{RowID: 40}}}
	sess.replayExhausted = true

	chain := sess.chainFor(time.Now(), 100)

	if chain.Origin != 39 {
		t.Fatalf("rotated chain rooted at %d, want strictly below the queue head (39)", chain.Origin)
	}
}

func TestListenPollIntervalShrinksBelowRetention(t *testing.T) {
	st := openChangeStore(t)
	sess := protectedSession(t, st)
	if got := sess.pollInterval(); got != listenPollInterval {
		t.Fatalf("default-retention poll interval = %v, want %v", got, listenPollInterval)
	}

	short, err := Open(t.TempDir(), WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open short-retention store: %v", err)
	}
	t.Cleanup(func() { short.Close() })
	s2 := testSession(nil)
	s2.s = short
	if got := s2.pollInterval(); got != 20*time.Millisecond {
		t.Fatalf("short-retention poll interval = %v, want 20ms (R/2)", got)
	}
}

func TestListenPollPumpProtectsParkedDrainClose(t *testing.T) {
	st := openStampedStore(t)
	old := time.Now().Add(-200 * time.Millisecond)
	seedStampedChanges(t, st, "notes", []time.Time{old, old, old})
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n
	sess.queue = []loggedChange{{seq: 1, rec: ChangeRecord{RowID: 1}}, {seq: 2, rec: ChangeRecord{RowID: 2}}}
	sess.liveRead = 2
	sess.pendingDrainClose = ErrListenLifetimeEnded

	sess.pumps.Add(1)
	go sess.pollWake()
	time.Sleep(150 * time.Millisecond)
	sess.endParked(nil)

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
	if seqs := changeSeqs(t, n); len(seqs) < 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("parked drain-close prefix after ticks + prune = %v, want the queued rows 1 and 2 retained", seqs)
	}
	sess.cancel()
}

func TestListenPollIntervalClampsAtOneMillisecond(t *testing.T) {
	nano, err := Open(t.TempDir(), WithChangeRetention(time.Nanosecond))
	if err != nil {
		t.Fatalf("open nano-retention store: %v", err)
	}
	t.Cleanup(func() { nano.Close() })
	sess := testSession(nil)
	sess.s = nano
	if got := sess.pollInterval(); got != time.Millisecond {
		t.Fatalf("nano-retention poll interval = %v, want the 1ms floor", got)
	}

	sess.pumps.Add(1)
	go sess.pollWake()
	time.Sleep(20 * time.Millisecond)
	sess.mu.Lock()
	dead := sess.dead
	sess.mu.Unlock()
	if dead {
		t.Fatal("the poll pump died on a nanosecond retention — the ticker panicked and recoverPump ended a healthy session")
	}
	sess.cancel()
}
