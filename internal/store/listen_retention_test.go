package store

import (
	"context"
	"testing"
	"time"
)

func TestPruneDueAdmitsOneCarrierPerInterval(t *testing.T) {
	st := openChangeStore(t)

	if !st.pruneDue("test", time.Now()) {
		t.Fatal("the first read in an interval was denied the prune carrier")
	}
	if st.pruneDue("test", time.Now()) {
		t.Fatal("a second read inside the interval took the carrier too")
	}
	time.Sleep(300 * time.Millisecond)
	if !st.pruneDue("test", time.Now()) {
		t.Fatal("the carrier did not free up after the interval")
	}
}

func TestPruneDueRetentionZeroNeverCarries(t *testing.T) {
	st, err := Open(t.TempDir(), WithChangeRetention(0))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	for i := 0; i < 3; i++ {
		if st.pruneDue("test", time.Now()) {
			t.Fatalf("retention-0 store minted a prune carrier on call %d", i)
		}
	}
}

func TestListenCarrierReadProtectsItsScan(t *testing.T) {
	st := openStampedStore(t)
	old := time.Now().Add(-200 * time.Millisecond)
	seedStampedChanges(t, st, "notes", []time.Time{old, old, old})
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)
	sess.replayExhausted = true

	scanned, ended, rerr := sess.readBatch(context.Background())
	if rerr != nil || ended {
		t.Fatalf("carrier read: ended=%v err=%v", ended, rerr)
	}
	if len(scanned) != 3 {
		t.Fatalf("carrier read scanned %d rows, want 3", len(scanned))
	}
	if seqs := changeSeqs(t, n); len(seqs) != 3 {
		t.Fatalf("the carrier's own prune deleted its scanned rows: %v", seqs)
	}
	rows, err := n.ro.QueryContext(context.Background(),
		`SELECT COUNT(*) FROM _dolmen_cursor_tokens WHERE position = 0`)
	if err != nil {
		t.Fatalf("count protective tokens: %v", err)
	}
	defer rows.Close()
	var tokens int
	for rows.Next() {
		if err := rows.Scan(&tokens); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	if tokens != 1 {
		t.Fatalf("protective tokens at the batch start = %d, want 1", tokens)
	}
}

func TestListenCarrierReadPrunesAgedTail(t *testing.T) {
	st := openStampedStore(t)
	old := time.Now().Add(-200 * time.Millisecond)
	seedStampedChanges(t, st, "notes", []time.Time{old, old, old})
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 3)
	sess.liveRead = 3
	sess.replayExhausted = true

	scanned, _, rerr := sess.readBatch(context.Background())
	if rerr != nil {
		t.Fatalf("carrier read: %v", rerr)
	}
	if len(scanned) != 0 {
		t.Fatalf("carrier read scanned %d rows past liveRead, want 0", len(scanned))
	}
	if seqs := changeSeqs(t, n); len(seqs) != 0 {
		t.Fatalf("aged rows behind the read survived the carrier prune: %v", seqs)
	}
}

func TestListenNonCarrierReadDelivers(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 1)

	got := make(chan ChangeRecord, 2)
	replay, cancel := listenOn(t, st, "", "", func(r ChangeRecord) { got <- r }, nil)
	defer cancel()
	drainReplay(t, replay)

	deadline := time.Now().Add(5 * time.Second)
	for !st.pruneDue("test", time.Now()) {
		if time.Now().After(deadline) {
			t.Fatal("could not consume the prune carrier slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	insertNotes(t, st, 1)

	select {
	case r := <-got:
		if r.RowID != int64(2) {
			t.Fatalf("read-only-path record arrived at row %d, want 2", r.RowID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the non-carrier read (read-only pool) never delivered the commit")
	}
}
