package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Slice 6b (r7a): the delivery mint. The drain (next slice) delivers
// queue entries one at a time, each carrying a cursor minted at ITS
// delivery moment; these pins hold the mint to its contract without the
// drain: a live record mints a resolvable token, and a record the log no
// longer holds fails loudly rather than minting a skipping cursor.

// TestListenMintOneMintsAtPosition: the delivery token resolves at the
// delivered record's position — the reconnect point the client needs.
func TestListenMintOneMintsAtPosition(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)

	tok, err := sess.mintOne(context.Background(), 1)
	if err != nil {
		t.Fatalf("mintOne: %v", err)
	}
	if tok == "" {
		t.Fatal("mintOne returned an empty token")
	}
	tx, err := n.rw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("resolve tx: %v", err)
	}
	defer tx.Rollback()
	row, err := resolveCursorToken(context.Background(), tx, time.Now(), st.changeRetention, tok, sess.table)
	if err != nil {
		t.Fatalf("resolve delivered token: %v", err)
	}
	if row.Position != 1 {
		t.Fatalf("delivered token resolves at %d, want 1", row.Position)
	}
}

// TestListenMintOneMissingFailsLoudly: a record deleted between the fill's
// scan and the delivery (a concurrent pruner's window) mints NOTHING —
// the teaching expiry, never a token whose reconnect silently skips the
// deleted tail.
func TestListenMintOneMissingFailsLoudly(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 2)

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open test: %v", err)
	}
	sess := testSession(nil)
	sess.s, sess.n = st, n
	sess.chain = newCursorChain(time.Now(), 0)

	if _, err := sess.mintOne(context.Background(), 999); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("mintOne at a missing position = %v, want ErrCursorExpired", err)
	}
}
