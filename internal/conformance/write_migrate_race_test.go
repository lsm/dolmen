package conformance

import (
	"fmt"
	"sync"
	"testing"
)

func TestWritesDoNotConflictWithConcurrentMigrations(t *testing.T) {
	h := newHarness(t)
	h.seedTable("race", "t", []map[string]any{{"name": "n", "type": "number"}})

	stop := make(chan struct{})
	var migrators sync.WaitGroup
	migrators.Add(1)
	go func() {
		defer migrators.Done()
		for i := 0; i < 40; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.httpCall("migrate", map[string]any{"namespace": "race", "table": "t", "changes": []map[string]any{
				{"op": "add_field", "field": map[string]any{"name": fmt.Sprintf("f%d", i), "type": "string"}},
			}})
		}
	}()

	var mu sync.Mutex
	conflicts := 0
	codeOf := func(out map[string]any) string {
		errEnv, _ := out["error"].(map[string]any)
		code, _ := errEnv["code"].(string)
		return code
	}
	run := func(op string, body map[string]any) {
		_, out := h.httpCall(op, body)
		if codeOf(out) == "conflict" {
			mu.Lock()
			conflicts++
			mu.Unlock()
		}
	}

	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 40; i++ {
				run("insert", map[string]any{"namespace": "race", "table": "t", "records": []map[string]any{{"n": w*1000 + i}}})
				run("update", map[string]any{"namespace": "race", "table": "t", "filter": "n = ?", "args": []any{w*1000 + i}, "set": map[string]any{"n": w*1000 + i}})
				run("upsert_by_key", map[string]any{"namespace": "race", "table": "t", "on": []string{"n"}, "records": []map[string]any{{"n": w*1000 + i}}})
			}
		}(w)
	}
	writers.Wait()
	close(stop)
	migrators.Wait()

	if conflicts != 0 {
		t.Fatalf("%d writes returned a spurious conflict while migrations ran; a benign schema version bump must retry, not conflict", conflicts)
	}
}
