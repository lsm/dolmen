package driver

import (
	"context"
	"os"
	"strings"
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

func TestAQueryWhoseWorkingSetExceedsTheCeilingSpillsOrIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("the memory ceiling test allocates")
	}
	ceiling := os.Getenv("SIDECAR_TIGHT_MEMORY")
	if ceiling == "" {
		t.Skip("SIDECAR_TIGHT_MEMORY is unset")
	}
	const rows = 10000000
	e := requireEngine(t)
	tbl, err := fixture.WriteBatched(context.Background(), t.TempDir(), rows, 250000)
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	root := rootOf(tbl)
	want := rows
	const heavy = "SELECT count(*) FROM (SELECT id, body FROM events GROUP BY id, body) AS g"

	for _, lock := range []bool{true, false} {
		env := []string{"SIDECAR_DATA_DIR=" + root, "SIDECAR_MEMORY_MAX=" + ceiling}
		if e.Ext != "" {
			env = append(env, "SIDECAR_EXT_DIR="+e.Ext)
		}
		if !lock {
			env = append(env, "SIDECAR_UNLOCKED=1")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
		sc, err := sidecar.Start(ctx, sidecar.Options{Bin: e.Bin, Env: env})
		if err != nil {
			cancel()
			t.Fatalf("start sidecar: %v", err)
		}
		if err := sc.Init(ctx, root, fixture.CurrentSnapshotID(tbl), "events", "app/events"); err != nil {
			cancel()
			_ = sc.Close()
			t.Fatalf("init: %v", err)
		}
		res, err := sc.Query(ctx, heavy)
		_ = sc.Close()
		cancel()
		if err != nil {
			lower := strings.ToLower(err.Error())
			switch {
			case strings.Contains(lower, "out of memory"),
				strings.Contains(lower, "memory limit"),
				strings.Contains(lower, "memory pool"),
				strings.Contains(lower, "resources exhausted"),
				strings.Contains(lower, "exceeds limit"),
				strings.Contains(lower, "insufficient"):
				t.Logf("MEMORY engine=%s locked=%v ceiling=%s groups=%d outcome=refused: %.200s", e.Name, lock, ceiling, want, err)
			default:
				t.Fatalf("locked=%v: the query failed for a reason that is not the memory ceiling: %v", lock, err)
			}
			continue
		}
		if got := countOf(t, res); got != want {
			t.Fatalf("locked=%v: the query returned %d groups under the ceiling, want %d", lock, got, want)
		}
		t.Logf("MEMORY engine=%s locked=%v ceiling=%s groups=%d outcome=completed", e.Name, lock, ceiling, want)
	}
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
