package blackbox

import (
	"fmt"
	"sync"
	"testing"
)

func TestStage05NotificationDaemon(t *testing.T) {
	stream := openSubscribe(t, scenarioNamespace, "begin")

	var replay []map[string]any
	lastCursor := ""
	var readyFrame sseFrame
	for readyFrame.Event == "" {
		f := stream.Next(t, "replay")
		switch f.Event {
		case "change":
			change := decodeChange(t, f.Data)
			replay = append(replay, change)
			lastCursor = asStr(t, change["cursor"], "replay change cursor")
		case "ready":
			readyFrame = f
		default:
			t.Fatalf("replay phase: unexpected frame %q", f.Event)
		}
	}
	if len(replay) < 40 {
		t.Fatalf("replay delivered %d changes, fewer than the 40 kb inserts alone", len(replay))
	}
	assertChangeOrder(t, replay, "replay")

	var readyPayload struct {
		Cursor string `json:"cursor"`
	}
	decodeInto(t, []byte(readyFrame.Data), &readyPayload, "ready frame")
	if readyPayload.Cursor == "" {
		t.Fatal("ready frame carries no cursor")
	}
	if readyPayload.Cursor != lastCursor {
		t.Fatalf("ready cursor %q does not match the replay tail cursor %q", readyPayload.Cursor, lastCursor)
	}

	writerCount := 5
	perWriter := 2
	var mu sync.Mutex
	liveExpected := map[int64]bool{}
	writeErr := make(chan error, writerCount*perWriter)
	var wg sync.WaitGroup
	for w := 0; w < writerCount; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				subject := fmt.Sprintf("Night shift ticket w%d-%d", w, j)
				data, err := opRaw("insert", map[string]any{
					"namespace": scenarioNamespace,
					"table":     "tickets",
					"records": []any{map[string]any{
						"subject": subject,
						"body":    "filed by a concurrent cli writer",
						"status":  "open",
					}},
				})
				if err != nil {
					writeErr <- err
					return
				}
				ids, _ := data["ids"].([]any)
				mu.Lock()
				for _, id := range ids {
					num, _ := id.(float64)
					liveExpected[int64(num)] = true
				}
				mu.Unlock()
			}
		}(w)
	}

	readTickets := func(s *sseStream, want int, what string) []map[string]any {
		var out []map[string]any
		for len(out) < want {
			out = append(out, s.NextChange(t, what))
		}
		return out
	}

	firstLeg := readTickets(stream, 4, "live first leg")
	killCursor := asStr(t, firstLeg[len(firstLeg)-1]["cursor"], "first leg tail cursor")
	stream.Close()
	wg.Wait()
	select {
	case err := <-writeErr:
		t.Fatal(err)
	default:
	}
	mu.Lock()
	expectedCount := len(liveExpected)
	mu.Unlock()
	if expectedCount != writerCount*perWriter {
		t.Fatalf("writers filed %d tickets, expected %d", expectedCount, writerCount*perWriter)
	}

	resumed, err := openSubscribeOn(app.srv.url, scenarioNamespace, killCursor)
	if err != nil {
		t.Fatalf("reconnect after the socket kill failed: %v", err)
	}

	var secondLeg []map[string]any
	for len(secondLeg) < expectedCount-len(firstLeg) {
		f := resumed.Next(t, "reconnect")
		if f.Event == "ready" {
			continue
		}
		if f.Event != "change" {
			t.Fatalf("reconnect phase: unexpected frame %q", f.Event)
		}
		secondLeg = append(secondLeg, decodeChange(t, f.Data))
	}

	delivered := append(append([]map[string]any{}, firstLeg...), secondLeg...)
	seen := map[int64]int{}
	for _, change := range delivered {
		seen[asInt(t, change["row_id"], "delivered row_id")]++
	}
	mu.Lock()
	defer mu.Unlock()
	for id := range liveExpected {
		n := seen[id]
		if n != 1 {
			t.Fatalf("ticket %d delivered %d times across the disconnect boundary, expected exactly once", id, n)
		}
	}
	if len(seen) != expectedCount {
		t.Fatalf("%d distinct tickets delivered across the boundary, expected %d", len(seen), expectedCount)
	}
	assertChangeOrder(t, delivered, "live across disconnect")

	app.daemon = resumed
	app.daemonLast = asStr(t, secondLeg[len(secondLeg)-1]["cursor"], "second leg tail cursor")
}

func decodeChange(t *testing.T, data string) map[string]any {
	t.Helper()
	var change map[string]any
	decodeInto(t, []byte(data), &change, "change frame")
	return change
}

func assertChangeOrder(t *testing.T, changes []map[string]any, what string) {
	t.Helper()
	seen := map[string]bool{}
	inserts := map[string][]int64{}
	for _, change := range changes {
		key := fmt.Sprintf("%v", change)
		if seen[key] {
			t.Fatalf("%s: change %v delivered twice", what, change)
		}
		seen[key] = true
		kind := asStr(t, change["kind"], "change kind")
		table := asStr(t, change["table"], "change table")
		if kind == "insert" {
			inserts[table] = append(inserts[table], asInt(t, change["row_id"], "insert row_id"))
		}
	}
	for table, ids := range inserts {
		for i := 1; i < len(ids); i++ {
			if ids[i] <= ids[i-1] {
				t.Fatalf("%s: %s insert row_ids are not in commit order: %v", what, table, ids)
			}
		}
	}
}
