package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/lsm/dolmen/spike/driver/fixture"
	"github.com/lsm/dolmen/spike/driver/sidecar"
)

type engine struct {
	Name string
	Bin  string
	Ext  string
}

func requireEngine(t *testing.T) engine {
	t.Helper()
	bin := os.Getenv("SIDECAR_BIN")
	if bin == "" {
		t.Skip("SIDECAR_BIN is unset; this suite runs in the sidecar CI jobs, not in the main graph")
	}
	name := os.Getenv("SIDECAR_ENGINE")
	if name == "" {
		name = filepath.Base(bin)
	}
	return engine{Name: name, Bin: bin, Ext: os.Getenv("SIDECAR_EXT_DIR")}
}

func newFixture(t *testing.T, rows int) *fixture.Table {
	t.Helper()
	dir := t.TempDir()
	tbl, err := fixture.Write(context.Background(), dir, rows)
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return tbl
}

func (e engine) start(t *testing.T, dir string, snapshot int64, table string, locked bool) *sidecar.Sidecar {
	t.Helper()
	env := []string{"SIDECAR_DATA_DIR=" + dir}
	if e.Ext != "" {
		env = append(env, "SIDECAR_EXT_DIR="+e.Ext)
	}
	if !locked {
		env = append(env, "SIDECAR_UNLOCKED=1")
	}
	if os.Getenv("SIDECAR_MEMORY_MAX") != "" {
		env = append(env, "SIDECAR_MEMORY_MAX="+os.Getenv("SIDECAR_MEMORY_MAX"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started, err := sidecar.Start(ctx, sidecar.Options{Bin: e.Bin, Env: env})
	if err != nil {
		t.Fatalf("start %s sidecar: %v", e.Name, err)
	}
	t.Cleanup(func() { _ = started.Close() })
	if err := started.Init(ctx, dir, snapshot, table, "app/events"); err != nil {
		t.Fatalf("%s sidecar init: %v", e.Name, err)
	}
	return started
}

func rootOf(tbl *fixture.Table) string {
	return tbl.DataDir[:len(tbl.DataDir)-len("/app/events")]
}

func locked(t *testing.T, tbl *fixture.Table) (*engine, *sidecar.Sidecar, string) {
	t.Helper()
	e := requireEngine(t)
	root := rootOf(tbl)
	return &e, e.start(t, root, fixture.CurrentSnapshotID(tbl), "events", true), root
}

func unlocked(t *testing.T, tbl *fixture.Table) (*engine, *sidecar.Sidecar, string) {
	t.Helper()
	e := requireEngine(t)
	root := rootOf(tbl)
	return &e, e.start(t, root, fixture.CurrentSnapshotID(tbl), "events", false), root
}

const attackBudget = 20 * time.Second

func query(t *testing.T, sc *sidecar.Sidecar, sql string) (*sidecar.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return sc.Query(ctx, sql)
}

func queryWithin(t *testing.T, sc *sidecar.Sidecar, sql string, d time.Duration) (*sidecar.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return sc.Query(ctx, sql)
}

func mustQuery(t *testing.T, sc *sidecar.Sidecar, sql string) *sidecar.Result {
	t.Helper()
	res, err := query(t, sc, sql)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return res
}

func rowsOf(res *sidecar.Result) []string {
	return res.Rows
}

func countOf(t *testing.T, res *sidecar.Result) int {
	t.Helper()
	if len(res.Rows) != 1 {
		t.Fatalf("want a single count row, got %q", res.Rows)
	}
	n, err := strconv.Atoi(res.Rows[0])
	if err != nil {
		t.Fatalf("count %q is not a number: %v", res.Rows[0], err)
	}
	return n
}

func errorClass(err error) string {
	var qe *sidecar.QueryError
	if errors.As(err, &qe) {
		return qe.Class
	}
	return "transport"
}

func tryInit(t *testing.T, e engine, root string, snapshot int64, table string) (*sidecar.Sidecar, error) {
	t.Helper()
	env := []string{"SIDECAR_DATA_DIR=" + root}
	if e.Ext != "" {
		env = append(env, "SIDECAR_EXT_DIR="+e.Ext)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started, err := sidecar.Start(ctx, sidecar.Options{Bin: e.Bin, Env: env})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = started.Close() })
	if err := started.Init(ctx, root, snapshot, table, "app/events"); err != nil {
		return started, err
	}
	return started, nil
}

func TestTheResponseChannelCarriesNothingButResponses(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, _ := locked(t, tbl)

	for i := 0; i < 5; i++ {
		if _, err := query(t, sc, "SELECT count(*) FROM events"); err != nil {
			t.Fatalf("query %d failed: %v", i, err)
		}
	}
	if n := sc.StrayLines(); n != 0 {
		t.Fatalf("%d lines on the response channel were not responses; the last was %q, and an engine writing its own logging to stdout desynchronises the framing", n, sc.LastStrayLine())
	}
}
