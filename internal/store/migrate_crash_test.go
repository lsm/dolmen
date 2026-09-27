package store

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

const migrateChildEnv = "DOLMEN_MIGRATE_CHILD_DIR"

const migrateChildEmbedded = "dolmen-migrate-child-embedded"

const migrateRows = 4000

var migrateChildBackfill = Embedder{Embed: migrateChildBatch, Identity: "fake-space"}

func migrateChildBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) > 0 {
		fmt.Fprintln(os.Stdout, migrateChildEmbedded, len(texts))
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(50 * time.Millisecond):
	}
	return fakeEmbed(ctx, texts)
}

func migrateVectorizeChange() []schema.Change {
	on := true
	return []schema.Change{{Op: schema.OpSetVectorize, Name: "body", Value: &on}}
}

func TestMigrateChildBackfillsUntilKilled(t *testing.T) {
	dir := os.Getenv(migrateChildEnv)
	if dir == "" {
		t.Skip("run by TestAKilledMigrateLeavesTheTableConsistent")
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.Migrate(context.Background(), "kmig", "t", migrateVectorizeChange(), migrateChildBackfill, Incarnation{Version: 1})
	t.Fatalf("the migration finished on its own (%v): the parent must have killed it first", err)
}

func TestAKilledMigrateLeavesTheTableConsistent(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a migration process")
	}
	dir := t.TempDir()
	seed, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := legacy(seed)
	mustNS(t, l, "kmig")
	if _, err := l.CreateTable(context.Background(), "kmig", "t", []schema.Field{
		{Name: "n", Type: schema.Number},
		{Name: "body", Type: schema.Text},
	}); err != nil {
		t.Fatal(err)
	}
	seeded := 0
	for start := 0; start < migrateRows; start += MaxRecordsPerInsert {
		batch := make([]map[string]any, 0, MaxRecordsPerInsert)
		for i := start; i < start+MaxRecordsPerInsert && i < migrateRows; i++ {
			batch = append(batch, map[string]any{"n": i, "body": fmt.Sprintf("row %d of the migration fixture", i)})
		}
		res, err := seed.Insert(context.Background(), "kmig", "t", batch, WriteOpts{}, testEmbed, nil, Incarnation{})
		if err != nil {
			t.Fatal(err)
		}
		seeded += len(res.Ids)
	}
	if seeded != migrateRows {
		t.Fatalf("seeded %d rows, want %d", seeded, migrateRows)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMigrateChildBackfillsUntilKilled$", "-test.count=1")
	cmd.Env = append(os.Environ(), migrateChildEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	gone := make(chan struct{})
	childEmbedded := make(chan int, 4096)
	go func() {
		scanner := bufio.NewScanner(stdout)
		batches := 0
		for scanner.Scan() {
			text := scanner.Text()
			if !strings.Contains(text, migrateChildEmbedded) {
				continue
			}
			var n int
			if _, err := fmt.Sscanf(text, migrateChildEmbedded+" %d", &n); err != nil {
				continue
			}
			childEmbedded <- n
			batches++
			if batches < 2 {
				continue
			}
			select {
			case <-ready:
			default:
				close(ready)
			}
		}
		close(gone)
	}()
	select {
	case <-ready:
	case <-gone:
		cmd.Wait()
		t.Fatal("the child exited without backfilling two batches: the fixture must still be migrating when the parent kills it")
	case <-time.After(60 * time.Second):
		cmd.Wait()
		t.Fatal("the child never reported a second embedding batch, so it never staged a page")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the migrating child: %v", err)
	}
	cmd.Wait()
	select {
	case <-gone:
	case <-time.After(10 * time.Second):
	}
	childRows := 0
	for drained := false; !drained; {
		select {
		case n := <-childEmbedded:
			childRows += n
		default:
			drained = true
		}
	}
	if childRows == 0 {
		t.Fatal("the child embedded nothing before it was killed, so the retry has nothing staged to continue from")
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("the namespace must reopen after its migration was killed: %v", err)
	}
	defer st.Close()
	survived := embedStageCount(t, st, "kmig", "t")
	ctx := context.Background()
	sc, _, err := st.DescribeTable(ctx, "kmig", "t", nil, Incarnation{})
	if err != nil {
		t.Fatalf("the table must be readable after the kill: %v", err)
	}
	if sc.Version != 1 && sc.Version != 2 {
		t.Fatalf("the schema version is %d, want the old 1 or the new 2, never anything between", sc.Version)
	}
	if sc.Version == 2 {
		if err := schema.Validate(sc.Fields); err != nil {
			t.Fatalf("a table stamped version 2 must carry a complete schema: %v", err)
		}
		if sc.VectorizeField() == nil {
			t.Fatalf("a table stamped version 2 must carry the vectorized field: %+v", sc.Fields)
		}
	}
	rows, err := st.Query(ctx, "kmig", "SELECT count(*) AS c, min(id) AS lo, max(id) AS hi FROM t", nil, [16]byte{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("the rows must be readable after the kill: %v", err)
	}
	if len(rows.Rows) != 1 {
		t.Fatalf("the count query returned %v", rows.Rows)
	}
	count, lo, hi := numberOf(t, rows.Rows[0], "c"), numberOf(t, rows.Rows[0], "lo"), numberOf(t, rows.Rows[0], "hi")
	if count != migrateRows || lo != 1 || hi != migrateRows {
		t.Fatalf("after the kill the table holds %d rows with ids %d..%d, want %d rows with ids 1..%d: a killed migration must lose nothing", count, lo, hi, migrateRows, migrateRows)
	}

	if sc.Version == 2 {
		if _, err := st.Migrate(ctx, "kmig", "t", migrateVectorizeChange(), testEmbed, Incarnation{Version: 2}); err != nil {
			t.Fatalf("vectorizing an already vectorized field is a no-op the engine must accept: %v", err)
		}
		return
	}
	retried := 0
	counting := Embedder{Identity: "fake-space", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		retried += len(texts)
		return fakeEmbed(ctx, texts)
	}}
	after, err := st.Migrate(ctx, "kmig", "t", migrateVectorizeChange(), counting, Incarnation{Version: 1})
	if err != nil {
		t.Fatalf("running the same migration again must succeed: %v", err)
	}
	if after.Version != 2 {
		t.Fatalf("the retried migration ended at version %d, want 2", after.Version)
	}
	if retried >= migrateRows {
		t.Fatalf("the retry embedded %d rows, want fewer than the %d the table holds: the rows the killed child staged must have survived the crash", retried, migrateRows)
	}
	if survived == 0 {
		t.Fatal("no staged rows survived the kill, so the retry had nothing to continue from")
	}
	if survived+int64(retried) != migrateRows {
		t.Fatalf("the kill left %d rows staged and the retry embedded %d, want %d between them: every row is embedded exactly once across the crash", survived, retried, migrateRows)
	}
	back, err := st.Query(ctx, "kmig", "SELECT count(*) AS c, min(id) AS lo, max(id) AS hi FROM t", nil, [16]byte{}, Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, lo, hi := numberOf(t, back.Rows[0], "c"), numberOf(t, back.Rows[0], "lo"), numberOf(t, back.Rows[0], "hi"); got != migrateRows || lo != 1 || hi != migrateRows {
		t.Fatalf("the retried migration left %d rows with ids %d..%d, want %d rows with ids 1..%d", got, lo, hi, migrateRows, migrateRows)
	}
}

func numberOf(t *testing.T, row map[string]any, key string) int64 {
	t.Helper()
	switch v := row[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		parsed, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			t.Fatalf("%s = %v is not an integer: %v", key, v, err)
		}
		return parsed
	}
	t.Fatalf("row %v has no integer at %q", row, key)
	return 0
}
