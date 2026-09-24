package store

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

const crashChildEnv = "DOLMEN_CRASH_CHILD_DIR"

func TestCrashChildWritesUntilKilled(t *testing.T) {
	dir := os.Getenv(crashChildEnv)
	if dir == "" {
		t.Skip("run by TestAKilledWriterLosesNothingAcknowledged")
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := bufio.NewWriter(os.Stdout)
	for i := 0; ; i++ {
		res, err := st.Insert(context.Background(), "crash", "t", []map[string]any{{"n": i}}, WriteOpts{}, testEmbed, nil, Incarnation{})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(out, "%d\n", res.Ids[0])
		out.Flush()
	}
}

func TestAKilledWriterLosesNothingAcknowledged(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a writer process")
	}
	dir := t.TempDir()
	seed, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := legacy(seed)
	mustNS(t, l, "crash")
	if _, err := l.CreateTable(context.Background(), "crash", "t", []schema.Field{{Name: "n", Type: schema.Number}}); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChildWritesUntilKilled$", "-test.count=1")
	cmd.Env = append(os.Environ(), crashChildEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	acked := map[int64]bool{}
	lines := bufio.NewScanner(stdout)
	for len(acked) < 300 && lines.Scan() {
		if id, err := strconv.ParseInt(lines.Text(), 10, 64); err == nil {
			acked[id] = true
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if len(acked) < 300 {
		t.Fatalf("the writer acknowledged only %d rows before it stopped", len(acked))
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("a namespace must reopen after its writer was killed: %v", err)
	}
	defer st.Close()
	n, err := st.ns("crash")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	var check string
	if err := n.ro.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity check after the kill: %q %v", check, err)
	}
	rows, err := n.ro.Query(`SELECT id FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	present := map[int64]bool{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		present[id] = true
	}
	for id := range acked {
		if !present[id] {
			t.Fatalf("row %d was acknowledged before the kill but is gone after reopen", id)
		}
	}
	var changes int
	if err := n.ro.QueryRow(`SELECT count(*) FROM _dolmen_changes WHERE table_name = 't'`).Scan(&changes); err != nil || changes < len(present) {
		t.Fatalf("every surviving row must have its change record: %d changes for %d rows (%v)", changes, len(present), err)
	}
}
