package driver

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/lsm/dolmen/spike/driver/fixture"
	"github.com/lsm/dolmen/spike/driver/sidecar"
)

const foreverQuery = "SELECT count(*) FROM range(20000000000)"

func TestALongQueryIsCancelledMidFlightRatherThanRunToCompletion(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, _ := locked(t, tbl)

	if _, err := query(t, sc, "SELECT count(*) FROM events"); err != nil {
		t.Fatalf("the sidecar cannot serve a trivial query before the cancel test: %v", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = sc.Cancel()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := sc.Query(ctx, foreverQuery)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a cancelled long query returned a result instead of being interrupted")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("the cancel took %s, so the query was not interrupted mid-flight", elapsed)
	}
	t.Logf("cancelled after %s with %v", elapsed, err)
}

func TestTheSidecarKeepsServingAfterAQueryWasCancelled(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, _ := locked(t, tbl)

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = sc.Cancel()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = sc.Query(ctx, foreverQuery)

	res := mustQuery(t, sc, "SELECT count(*) FROM events")
	if res.Rows == nil || res.Rows[0] == "" {
		t.Fatal("the sidecar stopped answering after a cancelled query")
	}
}

func TestAQueryThatOverrunsItsTimeoutFallsBackToKillingTheSidecar(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, _ := locked(t, tbl)
	pid := sc.Pid()
	if pid == 0 {
		t.Fatal("the sidecar has no pid, so a kill could not be verified")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := sc.Query(ctx, foreverQuery); err == nil {
		t.Fatal("the long query beat a 300ms deadline, so the timeout fallback was not exercised")
	}

	if err := sc.Kill(); err != nil {
		t.Logf("kill reported %v, which is expected if the query had already ended", err)
	}
	if processAlive(pid) {
		t.Fatalf("sidecar pid %d is still running after the kill fallback", pid)
	}
}

func TestAFreshSidecarServesCleanlyAfterThePreviousOneWasKilled(t *testing.T) {
	tbl := newFixture(t, 20)
	e := requireEngine(t)
	root := rootOf(tbl)

	first := e.start(t, root, fixture.CurrentSnapshotID(tbl), "events", true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _ = first.Query(ctx, foreverQuery)
	firstPid := first.Pid()
	_ = first.Kill()

	second := e.start(t, root, fixture.CurrentSnapshotID(tbl), "events", true)
	if second.Pid() == firstPid {
		t.Fatalf("the second sidecar reused pid %d, so the killed process was not really replaced", firstPid)
	}
	res := mustQuery(t, second, "SELECT count(*) FROM events")
	if got := countOf(t, res); got == 0 {
		t.Fatal("the fresh sidecar answered a count of zero for a table with rows")
	}
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
