package conformance

import (
	"fmt"
	"sync"
	"testing"
)

// Concurrency contract: many concurrent readers (query, search_fulltext,
// search_vector, describe_table) alongside a single writer (insert, update,
// upsert, delete). WAL mode allows the readers to proceed while the writer
// commits; every request must succeed (no locked-database errors surfaced as
// 5xx) and the final state must be exactly what the writer wrote.
//
// Run under `go test -race` (make test) this also exercises the server's
// shared state for data races.
func TestConcurrentReadersSingleWriter(t *testing.T) {
	h := newHarness(t)
	h.seedTable("race", "t", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "n", "type": "number"},
		{"name": "v", "type": "vector", "dim": 4},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "race", "table": "t",
		// n is -1 so no writer iteration's n = i filter ever matches the seed.
		"records": []map[string]any{{"title": "seed needle", "body": "seed body", "n": -1, "v": []any{1, 0, 0, 0}}},
	})

	const readerGoroutines = 6
	const writerIterations = 40

	readOps := []func() error{
		func() error { // full scan
			_, err := checkOK(h.httpCall("query", map[string]any{
				"namespace": "race", "sql": "SELECT id, title, n FROM t",
			}))
			return err
		},
		func() error { // fulltext
			_, err := checkOK(h.httpCall("search_fulltext", map[string]any{
				"namespace": "race", "table": "t", "query": "needle",
			}))
			return err
		},
		func() error { // raw vector search
			_, err := checkOK(h.httpCall("search_vector", map[string]any{
				"namespace": "race", "table": "t", "column": "v", "vector": []any{1, 0, 0, 0},
			}))
			return err
		},
		func() error { // text vector search (exercises the shared fake embedder)
			_, err := checkOK(h.httpCall("search_vector", map[string]any{
				"namespace": "race", "table": "t", "text": "seed body",
			}))
			return err
		},
		func() error { // schema read
			_, err := checkOK(h.httpCall("describe_table", map[string]any{
				"namespace": "race", "table": "t",
			}))
			return err
		},
		func() error { // MCP reader in the mix
			res := h.mcpCall("query", map[string]any{
				"namespace": "race", "sql": "SELECT count(*) AS c FROM t",
			})
			if res.status != 200 || res.proto != nil || res.isError() {
				return fmt.Errorf("mcp query failed: %+v", res)
			}
			return nil
		},
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers loop over every read op until the writer finishes.
	for i := 0; i < readerGoroutines; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := readOps[seed%len(readOps)](); err != nil {
					t.Errorf("concurrent read failed: %v", err)
					return
				}
			}
		}(i)
	}

	// One writer inserts, updates, upserts, and deletes.
	writer := func() (inserted, deleted int) {
		defer close(stop)
		for i := 0; i < writerIterations; i++ {
			rec := map[string]any{
				"title": fmt.Sprintf("row %d needle", i),
				"body":  fmt.Sprintf("body %d", i),
				"n":     i,
				"v":     []any{1, float64(i) / 100, 0, 0},
			}
			if _, err := checkOK(h.httpCall("insert", map[string]any{
				"namespace": "race", "table": "t", "records": []map[string]any{rec},
			})); err != nil {
				t.Errorf("concurrent insert %d failed: %v", i, err)
				return
			}
			inserted++
			upd, err := checkOK(h.httpCall("update", map[string]any{
				"namespace": "race", "table": "t", "filter": "n = ?",
				"args": []any{float64(i)},
				"set":  map[string]any{"n": i + 1000},
			}))
			if err != nil {
				t.Errorf("concurrent update %d failed: %v", i, err)
				return
			}
			if int64val(t, "concurrent update count", upd["updated"]) != 1 {
				t.Errorf("concurrent update %d reported %v, want 1", i, upd["updated"])
				return
			}
			ups, err := checkOK(h.httpCall("upsert", map[string]any{
				"namespace": "race", "table": "t", "filter": "n = ?", "args": []any{float64(i + 1000)},
				"set": map[string]any{"n": i + 2000},
			}))
			if err != nil {
				t.Errorf("concurrent upsert %d failed: %v", i, err)
				return
			}
			if int64val(t, "concurrent upsert count", ups["updated"]) != 1 || int64val(t, "concurrent upsert inserted", ups["inserted"]) != 0 {
				t.Errorf("concurrent upsert %d reported %v, want updated 1 / inserted 0", i, ups)
				return
			}
			// Every fourth row is deleted to interleave deletes with reads.
			if i%4 == 0 {
				del, err := checkOK(h.httpCall("delete", map[string]any{
					"namespace": "race", "table": "t", "filter": "n = ?", "args": []any{float64(i + 2000)},
				}))
				if err != nil {
					t.Errorf("concurrent delete %d failed: %v", i, err)
					return
				}
				if int64val(t, "concurrent delete count", del["deleted"]) != 1 {
					t.Errorf("concurrent delete %d reported %v, want 1", i, del["deleted"])
					return
				}
				deleted++
			}
		}
		return
	}
	inserted, deleted := writer()
	wg.Wait()
	if t.Failed() {
		return
	}

	// Final consistency: seed row + inserts − deletes.
	want := int64(1 + inserted - deleted)
	data := h.mustHTTP("describe_table", map[string]any{"namespace": "race", "table": "t"})
	if int64val(t, "final row count", data["row_count"]) != want {
		t.Fatalf("final row count %v, want %d (1 seed + %d inserted − %d deleted)",
			data["row_count"], want, inserted, deleted)
	}
	// The writes are durable and searchable after the churn.
	rows := h.mustHTTP("query", map[string]any{
		"namespace": "race", "sql": "SELECT count(*) AS c FROM t WHERE title LIKE 'row %'",
	})["rows"].([]any)
	got := int64val(t, "surviving rows", rows[0].(map[string]any)["c"])
	if got != want-1 {
		t.Fatalf("surviving written rows %d, want %d", got, want-1)
	}
}

// checkOK asserts an HTTP call succeeded and returns its data object.
func checkOK(status int, body map[string]any) (map[string]any, error) {
	if status != 200 || body["ok"] != true {
		return nil, fmt.Errorf("status %d: %v", status, body)
	}
	return body["data"].(map[string]any), nil
}
