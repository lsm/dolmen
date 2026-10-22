package driver

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/lsm/dolmen/spike/driver/fixture"
)

type queryShape struct {
	Name string
	SQL  string
}

func perfShapes() []queryShape {
	return []queryShape{
		{"full_scan_count", "SELECT count(*) FROM events"},
		{"full_scan_sum", "SELECT sum(score) FROM events"},
		{"selective_filter", "SELECT count(*) FROM events WHERE grp = 'beta' AND id < 1000000"},
		{"group_by", "SELECT grp, count(*) AS n, avg(score) AS avg_score FROM events GROUP BY grp ORDER BY grp"},
		{"join", "SELECT count(*) FROM events a JOIN events b ON a.id = b.id WHERE a.grp = 'alpha'"},
	}
}

func perfRows(t *testing.T) int {
	t.Helper()
	n := 10000000
	if v := os.Getenv("DOLMEN_SPIKE_ROWS"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("DOLMEN_SPIKE_ROWS=%q is not a number", v)
		}
		n = parsed
	}
	return n
}

func TestThePerfTableIsWrittenOnceAndEveryShapeIsMeasuredOnIt(t *testing.T) {
	if os.Getenv("DOLMEN_SPIKE_PERF") != "1" {
		t.Skip("set DOLMEN_SPIKE_PERF=1 to run the perf set; timings must come from CI")
	}
	e := requireEngine(t)
	rows := perfRows(t)

	dir := t.TempDir()
	writeStart := time.Now()
	tbl, err := fixture.WriteBatched(context.Background(), dir, rows, 250000)
	if err != nil {
		t.Fatalf("write the perf fixture: %v", err)
	}
	t.Logf("fixture: %d rows written in %s", rows, time.Since(writeStart).Round(time.Millisecond))

	sc := e.start(t, dir, fixture.CurrentSnapshotID(tbl), "events", true)

	baseline := mustQuery(t, sc, "SELECT count(*) FROM events")
	if got := countOf(t, baseline); got != rows {
		t.Fatalf("the perf table holds %d rows, want %d", got, rows)
	}

	t.Logf("engine=%s rows=%d", e.Name, rows)
	for _, shape := range perfShapes() {
		start := time.Now()
		res, err := query(t, sc, shape.SQL)
		elapsed := time.Since(start)
		if err != nil {
			t.Errorf("%s failed after %s: %v", shape.Name, elapsed, err)
			continue
		}
		first := ""
		if len(res.Rows) > 0 {
			first = res.Rows[0]
		}
		t.Logf("PERF engine=%s shape=%s wall=%s rows=%d first=%s",
			e.Name, shape.Name, elapsed.Round(time.Millisecond), len(res.Rows), first)
	}
}

func TestTheSidecarStartsAndAnswersItsFirstQueryQuickly(t *testing.T) {
	if os.Getenv("DOLMEN_SPIKE_PERF") != "1" {
		t.Skip("set DOLMEN_SPIKE_PERF=1 to run the perf set; timings must come from CI")
	}
	e := requireEngine(t)
	tbl := newFixture(t, 1000)
	root := rootOf(tbl)

	start := time.Now()
	sc := e.start(t, root, fixture.CurrentSnapshotID(tbl), "events", true)
	res := mustQuery(t, sc, "SELECT count(*) FROM events")
	t.Logf("COLDSTART engine=%s toFirstAnswer=%s rows=%s",
		e.Name, time.Since(start).Round(time.Millisecond), res.Rows[0])
}
