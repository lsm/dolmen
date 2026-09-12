package blackbox

import (
	"sync"
	"testing"
)

func TestStage10ConcurrentLoadSanity(t *testing.T) {
	op(t, "create_table", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "load",
		"fields": []any{
			map[string]any{"name": "writer", "type": "number"},
			map[string]any{"name": "seq", "type": "number"},
		},
	})

	head := op(t, "changes_since", map[string]any{"namespace": scenarioNamespace})
	headCursor := asStr(t, head["next_cursor"], "load head cursor")

	stream, err := openSubscribeOn(app.srv.url, scenarioNamespace, headCursor)
	if err != nil {
		t.Fatalf("subscriber before the load: %v", err)
	}
	t.Cleanup(func() { stream.Close() })

	writers := 8
	perWriter := 25
	total := writers * perWriter
	var mu sync.Mutex
	expected := map[int64]bool{}
	failures := make(chan error, writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				data, err := opRaw("insert", map[string]any{
					"namespace": scenarioNamespace,
					"table":     "load",
					"records": []any{map[string]any{
						"writer": float64(w),
						"seq":    float64(j),
					}},
				})
				if err != nil {
					failures <- err
					return
				}
				ids, _ := data["ids"].([]any)
				mu.Lock()
				for _, id := range ids {
					num, _ := id.(float64)
					expected[int64(num)] = true
				}
				mu.Unlock()
			}
		}(w)
	}

	var delivered []map[string]any
	for len(delivered) < total {
		f := stream.Next(t, "load delivery")
		if f.Event == "ready" {
			continue
		}
		if f.Event != "change" {
			t.Fatalf("load stream: unexpected frame %q", f.Event)
		}
		change := decodeChange(t, f.Data)
		if asStr(t, change["table"], "load table") != "load" {
			continue
		}
		delivered = append(delivered, change)
	}
	wg.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}

	if len(delivered) != total {
		t.Fatalf("stream delivered %d load changes, expected %d", len(delivered), total)
	}
	seen := map[int64]int{}
	for _, change := range delivered {
		seen[asInt(t, change["row_id"], "load row_id")]++
	}
	mu.Lock()
	defer mu.Unlock()
	if len(expected) != total {
		t.Fatalf("writers confirmed %d distinct ids, expected %d", len(expected), total)
	}
	for id := range expected {
		if seen[id] != 1 {
			t.Fatalf("load row %d delivered %d times, expected exactly once", id, seen[id])
		}
	}
	if len(seen) != total {
		t.Fatalf("%d distinct rows delivered, expected %d", len(seen), total)
	}
	assertChangeOrder(t, delivered, "load stream")

	describe := describeTable(t, "load")
	if rc := asInt(t, describe["row_count"], "load row_count"); rc != int64(total) {
		t.Fatalf("describe_table reports %d load rows, expected %d", rc, total)
	}
	tables := op(t, "list_tables", map[string]any{"namespace": scenarioNamespace})
	names, _ := tables["tables"].([]any)
	found := false
	for _, n := range names {
		if s, _ := n.(string); s == "load" {
			found = true
		}
	}
	if !found {
		t.Fatalf("list_tables omits the load table: %v", names)
	}
}
