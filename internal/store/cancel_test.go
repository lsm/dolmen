package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/telemetry/dbspan"
)

const cancelGrace = 30 * time.Second

const longRecursiveQuery = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000000) SELECT count(*) FROM c`

const shortRecursiveQuery = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 20000) SELECT count(*) FROM c`

func mustCancelInTime(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%s returned %v, want context.Canceled", what, err)
	}
	if got := SpanErrorType(err); got != string(derr.Canceled) {
		t.Fatalf("%s classified as %q, want canceled", what, got)
	}
}

func seedCancelFixture(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	if err := legacy(st).CreateNamespace("cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "cancel", "t", []schema.Field{
		{Name: "n", Type: schema.Number},
		{Name: "body", Type: schema.Text, Vectorize: true},
	}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < 400; start += MaxRecordsPerInsert {
		batch := make([]map[string]any, 0, MaxRecordsPerInsert)
		for i := start; i < start+MaxRecordsPerInsert && i < 400; i++ {
			batch = append(batch, map[string]any{"n": i, "body": fmt.Sprintf("row %d", i)})
		}
		if _, err := st.Insert(ctx, "cancel", "t", batch, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
}

func countOf(t *testing.T, st *Store, query string) int64 {
	t.Helper()
	rows, err := st.Query(context.Background(), "cancel", query, nil, [16]byte{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if len(rows.Rows) != 1 {
		t.Fatalf("%s returned %v", query, rows.Rows)
	}
	switch v := rows.Rows[0]["c"].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	t.Fatalf("row %v has no integer count", rows.Rows[0])
	return 0
}

func awaitSignal(t *testing.T, what string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(cancelGrace):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitDone(t *testing.T, what string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(cancelGrace):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func openTracedCancelStore(t *testing.T) (*Store, *startHook) {
	t.Helper()
	hook := &startHook{SpanProcessor: tracetest.NewSpanRecorder(), onName: map[string]chan struct{}{}}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(hook))
	st, err := Open(t.TempDir(), WithTracerProvider(tp), WithVectorCacheBytes(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	seedCancelFixture(t, st)
	return st, hook
}

func TestCancellingAnInsertWaitingForTheWriterReleasesIt(t *testing.T) {
	st, hook := openTracedCancelStore(t)
	n, err := st.ns("cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	held, err := n.rw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	waiting := hook.when(dbspan.WriterWait)
	inserted := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := st.Insert(ctx, "cancel", "t", []map[string]any{{"n": 1, "body": "blocked on the writer"}}, WriteOpts{}, testEmbed, nil, Incarnation{})
		inserted <- err
	}()
	awaitSignal(t, "the insert to reach the writer wait", waiting)
	cancel()
	mustCancelInTime(t, "an insert cancelled while waiting for the writer", awaitDone(t, "the cancelled insert", inserted))
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(context.Background(), "cancel", "t", []map[string]any{{"n": 1, "body": "after the cancel"}}, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatalf("the writer must be usable after a cancelled wait: %v", err)
	}
}

func TestAnInterruptedStatementIsNotRecognisedAsAQueryError(t *testing.T) {
	err := NewQueryError("SELECT 1", errors.New("interrupted (9)"))
	var qe *QueryError
	if errors.As(err, &qe) {
		t.Fatal("the driver's interrupt must not be dressed up as a query error: the changelog and this PR's premise both depend on it")
	}
	if got := SpanErrorType(err); got != "internal_error" {
		t.Fatalf("an unrecognised driver error classified as %q, want internal_error", got)
	}
}

func TestCancellingALongQueryReleasesTheReadPool(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedCancelFixture(t, st)
	n, err := st.ns("cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()

	over := readConnsPerNS + 1
	cancels := make([]context.CancelFunc, over)
	results := make([]chan error, over)
	for i := range over {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		results[i] = make(chan error, 1)
		go func(done chan error) {
			_, err := st.Query(ctx, "cancel", longRecursiveQuery, nil, [16]byte{}, Page{Limit: 10})
			done <- err
		}(results[i])
	}
	awaitSaturation(t, n)
	for _, cancel := range cancels {
		cancel()
	}
	for i, done := range results {
		err := awaitDone(t, fmt.Sprintf("cancelled query %d", i), done)
		if err == nil {
			continue
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled query %d returned %v, want context.Canceled: an interrupted statement is the caller's cancellation, not a query error", i, err)
		}
		if got := SpanErrorType(err); got != string(derr.Canceled) {
			t.Fatalf("cancelled query %d classified as %q, want canceled", i, got)
		}
	}
	after := make(chan error, 1)
	go func() {
		_, err := st.Query(context.Background(), "cancel", "SELECT count(*) AS c FROM t", nil, [16]byte{}, Page{Limit: 10})
		after <- err
	}()
	if err := awaitDone(t, "a normal query after the cancelled fan-out", after); err != nil {
		t.Fatalf("a normal query must still succeed after %d cancelled ones: %v", over, err)
	}
	if got := n.ro.Stats().InUse; got != 0 {
		t.Errorf("%d read connections are still in use after every cancelled query returned", got)
	}
	if got := countOf(t, st, "SELECT count(*) AS c FROM t"); got != 400 {
		t.Fatalf("the cancelled queries changed the row count to %d", got)
	}
}

func TestCancellingAQueryThatHasToBeRetriedIsStillCanceled(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedCancelFixture(t, st)
	n, err := st.ns("cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	rows, err := st.Query(context.Background(), "cancel", shortRecursiveQuery+` ORDER BY 1 LIMIT 5 OFFSET 0`, nil, [16]byte{}, Page{Limit: 10})
	if err != nil || len(rows.Rows) != 1 {
		t.Fatalf("a query carrying its own paging must still answer through the retry path: %v %v", rows, err)
	}
	for range 6 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := st.Query(ctx, "cancel", shortRecursiveQuery+` ORDER BY 1 LIMIT 5 OFFSET 0`, nil, [16]byte{}, Page{Limit: 10})
			done <- err
		}()
		awaitBusy(t, n)
		cancel()
		err := awaitDone(t, "the cancelled retried query", done)
		if err == nil {
			continue
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled query that had to be retried returned %v, want context.Canceled", err)
		}
		if got := SpanErrorType(err); got != string(derr.Canceled) {
			t.Fatalf("a cancelled retried query classified as %q, want canceled", got)
		}
	}
}

func awaitBusy(t *testing.T, n *nsDB) {
	t.Helper()
	deadline := time.Now().Add(cancelGrace)
	for n.ro.Stats().InUse == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the query never took a read connection")
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func awaitSaturation(t *testing.T, n *nsDB) {
	t.Helper()
	deadline := time.Now().Add(cancelGrace)
	for n.ro.Stats().InUse < readConnsPerNS {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of the %d read connections are in use; the fan-out never saturated the pool", n.ro.Stats().InUse, readConnsPerNS)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestACancellationOutranksAFaultAndNeverHidesOne(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	fault := NewQueryError("SELECT * FROM (SELECT 1 LIMIT 5) LIMIT ? OFFSET ?", errors.New("interrupted (9)"))
	if got := cancelled(done, fault); !errors.Is(got, context.Canceled) {
		t.Fatalf("a fault on a finished context reported as %v, want context.Canceled", got)
	}
	if got := cancelled(done, invalidf("refuse rowid hints on a masked table")); !errors.Is(got, context.Canceled) {
		t.Fatalf("a refusal on a finished context reported as %v, want context.Canceled", got)
	}
	live := NewQueryError("SELECT 1", errors.New("no such column: nope"))
	got := cancelled(context.Background(), live)
	if !errors.Is(got, live) {
		t.Fatalf("a real fault with a live context became %v, want the fault itself", got)
	}
	if SpanErrorType(got) != "query_error" {
		t.Fatalf("a real fault classified as %q, want query_error", SpanErrorType(got))
	}
}

func TestCancellingAnEmbeddingCallWritesNothingAndKeepsTheWriter(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedCancelFixture(t, st)
	before := countOf(t, st, "SELECT count(*) AS c FROM t")

	embedding := make(chan struct{})
	blocking := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		close(embedding)
		<-ctx.Done()
		return nil, ctx.Err()
	}, Identity: "fake-space"}
	ctx, cancel := context.WithCancel(context.Background())
	inserted := make(chan error, 1)
	go func() {
		_, err := st.Insert(ctx, "cancel", "t", []map[string]any{{"n": 9999, "body": "never embedded"}}, WriteOpts{}, blocking, nil, Incarnation{})
		inserted <- err
	}()
	awaitSignal(t, "the insert to reach its embedding call", embedding)
	cancel()
	mustCancelInTime(t, "an insert cancelled inside its embedding call", awaitDone(t, "the cancelled insert", inserted))
	if got := countOf(t, st, "SELECT count(*) AS c FROM t"); got != before {
		t.Fatalf("the cancelled insert wrote rows: %d before, %d after", before, got)
	}
	if _, err := st.Insert(context.Background(), "cancel", "t", []map[string]any{{"n": 1, "body": "after the cancel"}}, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatalf("the writer must be free after a cancelled embedding call: %v", err)
	}
	if got := countOf(t, st, "SELECT count(*) AS c FROM t"); got != before+1 {
		t.Fatalf("the insert after the cancel wrote %d rows, want 1", got-before)
	}
}
