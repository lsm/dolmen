package conformance

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func expectRows(w store.BatchWrite, n int64) store.BatchWrite {
	w.ExpectedRows = &n
	return w
}

func conditionCount(t *testing.T, h *harness, table, filter string) float64 {
	t.Helper()
	out := h.mustHTTP("query", map[string]any{"namespace": "acme", "sql": "SELECT count(*) AS n FROM " + table + " WHERE " + filter})
	return out["rows"].([]any)[0].(map[string]any)["n"].(float64)
}

func conditionJobs(t *testing.T, h *harness) {
	t.Helper()
	h.seedTable("acme", "jobs", []map[string]any{{"name": "title"}, {"name": "version", "type": "number"}})
	h.seedTable("acme", "events", []map[string]any{{"name": "job"}})
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "jobs", "records": []any{map[string]any{"title": "job", "version": 7}}})
}

func claimWrites() []store.BatchWrite {
	return []store.BatchWrite{
		expectRows(store.BatchWrite{Kind: store.BatchWriteUpdate, Table: "jobs", Filter: "title = ? AND version = ?", Args: []any{"job", 7}, Set: map[string]any{"version": 8}}, 1),
		{Kind: store.BatchWriteInsert, Table: "events", Records: []map[string]any{{"job": "job"}}},
	}
}

func runBatch(t *testing.T, h *harness, writes []store.BatchWrite, opts store.BatchOpts) (store.BatchResult, error) {
	t.Helper()
	return h.st.Batch(t.Context(), "acme", writes, opts, store.Embedder{}, nil, store.Incarnation{})
}

func TestBatchExpectedRowsMismatchRollsBackEveryEngine(t *testing.T) {
	for _, kind := range []store.BatchWriteKind{store.BatchWriteUpdate, store.BatchWriteDelete} {
		t.Run(string(kind), func(t *testing.T) {
			h := newHarness(t)
			batchDocs(t, h)
			w := store.BatchWrite{Kind: kind, Table: "docs", Filter: "title = 'absent'"}
			if kind == store.BatchWriteUpdate {
				w.Set = map[string]any{"body": "x"}
			}
			opts := store.BatchOpts{IdempotencyKey: "claim"}
			writes := []store.BatchWrite{{Kind: store.BatchWriteInsert, Table: "docs", Records: []map[string]any{{"title": "must-rollback"}}}, expectRows(w, 1)}
			r, err := runBatch(t, h, writes, opts)
			if !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "writes[1]: expected_rows mismatch") || len(r.Results) != 0 {
				t.Fatalf("stale condition = %+v %v, want an indexed conflict and no partial result", r, err)
			}
			if n := conditionCount(t, h, "docs", "1=1"); n != 0 {
				t.Fatalf("a failed condition left %v rows", n)
			}
			feed, _, err := h.st.ChangesSince(t.Context(), "acme", "", store.CursorBegin, [16]byte{}, nil, store.Incarnation{}, store.Page{})
			if err != nil || len(feed) != 0 {
				t.Fatalf("a failed condition left change-feed records: %+v %v", feed, err)
			}
			writes[1].Filter = "title = 'must-rollback'"
			if r, err := runBatch(t, h, writes, opts); err != nil || r.Replayed {
				t.Fatalf("a failed condition consumed the idempotency key: %+v %v", r, err)
			}
		})
	}
}

func TestBatchExpectedRowsCompareAndSetEveryEngine(t *testing.T) {
	h := newHarness(t)
	conditionJobs(t, h)
	opts := store.BatchOpts{IdempotencyKey: "version-seven"}
	first, err := runBatch(t, h, claimWrites(), opts)
	if err != nil || first.Results[0].Updated != 1 {
		t.Fatalf("matching claim = %+v %v", first, err)
	}
	replay, err := runBatch(t, h, claimWrites(), opts)
	if err != nil || !replay.Replayed {
		t.Fatalf("an exact replay after the state changed must return the saved result: %+v %v", replay, err)
	}
	changed := claimWrites()
	changed[0] = expectRows(changed[0], 0)
	if _, err := runBatch(t, h, changed, opts); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("a changed expectation under the same key must conflict: %v", err)
	}
	if _, err := runBatch(t, h, claimWrites(), store.BatchOpts{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("a stale version-7 claim must conflict: %v", err)
	}
	if n := conditionCount(t, h, "events", "1=1"); n != 1 {
		t.Fatalf("events = %v, want only the winning claim's", n)
	}
}

func TestBatchExpectedRowsZeroOmissionAndOrderEveryEngine(t *testing.T) {
	h := newHarness(t)
	conditionJobs(t, h)
	none := store.BatchWrite{Kind: store.BatchWriteDelete, Table: "jobs", Filter: "title = 'absent'"}
	if _, err := runBatch(t, h, []store.BatchWrite{none, expectRows(none, 0)}, store.BatchOpts{}); err != nil {
		t.Fatalf("omission and an asserted zero must both accept a zero match: %v", err)
	}
	same := expectRows(store.BatchWrite{Kind: store.BatchWriteUpdate, Table: "jobs", Filter: "title = 'job'", Set: map[string]any{"version": 7}}, 1)
	if _, err := runBatch(t, h, []store.BatchWrite{same}, store.BatchOpts{}); err != nil {
		t.Fatalf("an update that leaves values unchanged still matches its row: %v", err)
	}
	if _, err := runBatch(t, h, []store.BatchWrite{expectRows(store.BatchWrite{Kind: store.BatchWriteDelete, Table: "jobs", Filter: "1=1"}, 0)}, store.BatchOpts{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("an asserted zero must refuse a match: %v", err)
	}
	ordered := []store.BatchWrite{
		{Kind: store.BatchWriteInsert, Table: "jobs", Records: []map[string]any{{"title": "fresh", "version": 1}}},
		expectRows(store.BatchWrite{Kind: store.BatchWriteUpdate, Table: "jobs", Filter: "title = 'fresh'", Set: map[string]any{"version": 2}}, 1),
	}
	if _, err := runBatch(t, h, ordered, store.BatchOpts{}); err != nil {
		t.Fatalf("a condition must see the batch's earlier writes: %v", err)
	}
}

func TestBatchExpectedRowsCountsOnlyVisibleRowsEveryEngine(t *testing.T) {
	h := newHarness(t)
	h.ensureNS("acme")
	if _, err := h.st.CreateTable(t.Context(), "acme", "owned", []schema.Field{{Name: "score", Type: schema.Number}}, store.TableOpts{RowAccess: "own"}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.Insert(t.Context(), "acme", "owned", []map[string]any{{"score": 7}}, store.WriteOpts{Owner: "bob"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	w := store.BatchWrite{Kind: store.BatchWriteDelete, Table: "owned", Filter: "1=1", Scope: &store.RowScope{Owner: "alice"}}
	if _, err := runBatch(t, h, []store.BatchWrite{expectRows(w, 0)}, store.BatchOpts{}); err != nil {
		t.Fatalf("a row hidden from the caller must not count: %v", err)
	}
	if _, err := runBatch(t, h, []store.BatchWrite{expectRows(w, 1)}, store.BatchOpts{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("a hidden row satisfied the condition: %v", err)
	}
	if n := conditionCount(t, h, "owned", "1=1"); n != 1 {
		t.Fatalf("the hidden row changed: %v", n)
	}
}

func TestBatchExpectedRowsConcurrentClaimsCommitOneEventEveryEngine(t *testing.T) {
	h := newHarness(t)
	conditionJobs(t, h)
	start := make(chan struct{})
	out := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := h.st.Batch(context.Background(), "acme", claimWrites(), store.BatchOpts{}, store.Embedder{}, nil, store.Incarnation{})
			out <- err
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	wins, conflicts := 0, 0
	for err := range out {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, derr.ErrConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 || conditionCount(t, h, "events", "1=1") != 1 {
		t.Fatalf("two version-7 claims: wins=%d conflicts=%d, want exactly one committed event", wins, conflicts)
	}
}

func TestBatchExpectedRowsNotYetExposedOnWire(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	body := map[string]any{"namespace": "acme", "writes": []any{
		map[string]any{"kind": "insert", "table": "docs", "records": []any{map[string]any{"title": "must-rollback"}}},
		map[string]any{"kind": "delete", "table": "docs", "filter": "1=1", "expected_rows": 0},
	}}
	status, out := h.httpCall("batch", body)
	if status != http.StatusBadRequest || !strings.Contains(out["error"].(map[string]any)["message"].(string), "expected_rows") {
		t.Fatalf("wire admitted hidden engine feature: %d %+v", status, out)
	}
	result := h.mcpCall("batch", body)
	if !result.isError() || !strings.Contains(result.toolError()["message"].(string), "expected_rows") {
		t.Fatalf("MCP admitted hidden engine feature: %+v", result)
	}
	rows := h.mustHTTP("query", map[string]any{"namespace": "acme", "sql": "SELECT count(*) AS n FROM docs"})
	if rows["rows"].([]any)[0].(map[string]any)["n"] != float64(0) {
		t.Fatalf("rejected wire request wrote rows: %+v", rows)
	}
}
