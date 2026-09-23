package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestEachSyncModeReachesTheWriter(t *testing.T) {
	for _, c := range []struct {
		mode SyncMode
		want int
	}{{SyncFull, 2}, {SyncNormal, 1}} {
		st, err := Open(t.TempDir(), WithSync(c.mode))
		if err != nil {
			t.Fatal(err)
		}
		l := legacy(st)
		mustNS(t, l, "test")
		n, err := st.ns("test")
		if err != nil {
			t.Fatal(err)
		}
		var got int
		if err := n.rw.QueryRow(`PRAGMA synchronous`).Scan(&got); err != nil || got != c.want {
			t.Fatalf("sync %s: writer runs synchronous=%d (%v), want %d", c.mode, got, err, c.want)
		}
		st.Close()
	}
}

func TestTheDefaultSyncModeIsFull(t *testing.T) {
	if DefaultSync != SyncFull {
		t.Fatalf("default sync %s; an acknowledged commit must survive power loss unless the operator opts out", DefaultSync)
	}
}

func TestOpenRefusesAnUnknownSyncMode(t *testing.T) {
	if _, err := Open(t.TempDir(), WithSync("off")); err == nil || !strings.Contains(err.Error(), "use full") {
		t.Fatalf("got %v", err)
	}
}

func TestANamespaceNotRunningTheChosenModeIsRefused(t *testing.T) {
	path := t.TempDir() + "/x.db"
	db, err := sql.Open("sqlite", writerDSN(path, SyncNormal)+"&_pragma=synchronous(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := verifySync(context.Background(), db, "x", SyncNormal); err == nil {
		t.Fatal("a writer that did not take the chosen mode must be refused, not silently weaker")
	}
}
