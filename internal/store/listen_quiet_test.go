package store

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestQueueProtectionStaysQuietOnceItsSessionIsGone(t *testing.T) {
	st := openStore(t)
	mustCreateNotes(t, st)
	n, err := st.ns("test")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(old) })

	ctx, cancel := context.WithCancel(context.Background())
	sess := &listenSession{s: st.Store, n: n, nsName: "test", ctx: ctx, queue: []loggedChange{{seq: 7}}}
	st.changeRetention = time.Hour
	cancel()
	sess.protectQueue()
	if buf.Len() != 0 {
		t.Fatalf("a session torn down by its own cancellation logged an error: %s", buf.String())
	}

	live := &listenSession{s: st.Store, n: n, nsName: "test", ctx: context.Background(), queue: []loggedChange{{seq: 7}}}
	n.rw.Close()
	live.protectQueue()
	if buf.Len() == 0 {
		t.Fatal("a live session whose protection fails must still log it, or the quiet case above proves nothing")
	}
}
