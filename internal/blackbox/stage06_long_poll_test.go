package blackbox

import (
	"testing"
	"time"
)

func TestStage06AgentLongPoll(t *testing.T) {
	head := mcpTool(t, "wait_for", map[string]any{
		"namespace":  scenarioNamespace,
		"timeout_ms": 0,
	})
	headCursor := asStr(t, head["next_cursor"], "head next_cursor")
	if changes, _ := head["changes"].([]any); len(changes) != 0 {
		t.Fatalf("conditional wait at the head delivered %v", head)
	}

	wake := make(chan struct {
		data map[string]any
		err  error
	}, 1)
	go func() {
		data, err := mcpToolRaw("wait_for", map[string]any{
			"namespace":  scenarioNamespace,
			"cursor":     headCursor,
			"timeout_ms": 4000,
		})
		wake <- struct {
			data map[string]any
			err  error
		}{data, err}
	}()

	time.Sleep(250 * time.Millisecond)
	committed := op(t, "insert", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "events",
		"records": []any{
			map[string]any{"kind": "long_poll_probe", "detail": "commit during the wait"},
		},
	})
	commitID := asInt(t, committed["ids"].([]any)[0], "probe event id")
	app.probeID = commitID

	start := time.Now()
	select {
	case p := <-wake:
		if p.err != nil {
			t.Fatal(p.err)
		}
		if time.Since(start) > 3500*time.Millisecond {
			t.Fatalf("the wake took %v; it rode the timeout instead of the commit", time.Since(start))
		}
		changes, _ := p.data["changes"].([]any)
		if len(changes) != 1 {
			t.Fatalf("the wake page carries %d changes, expected exactly the one commit: %v", len(changes), p.data)
		}
		change, _ := changes[0].(map[string]any)
		if asInt(t, change["row_id"], "wake row_id") != commitID || asStr(t, change["table"], "wake table") != "events" {
			t.Fatalf("the wake delivered %v, expected the events row %d", change, commitID)
		}
		next := asStr(t, p.data["next_cursor"], "wake next_cursor")
		if next == "" || next == headCursor {
			t.Fatalf("the wake page did not advance the cursor: %q", next)
		}
		headCursor = next
	case <-time.After(6 * time.Second):
		t.Fatal("wait_for did not return after the commit")
	}

	idleStart := time.Now()
	idle := mcpTool(t, "wait_for", map[string]any{
		"namespace":  scenarioNamespace,
		"cursor":     headCursor,
		"timeout_ms": 1200,
	})
	if time.Since(idleStart) < time.Second {
		t.Fatalf("the idle wait returned in %v without blocking", time.Since(idleStart))
	}
	if changes, _ := idle["changes"].([]any); len(changes) != 0 {
		t.Fatalf("idle timeout page carries changes: %v", idle)
	}
	if next := asStr(t, idle["next_cursor"], "idle next_cursor"); next != headCursor {
		t.Fatalf("idle timeout moved the cursor: %q -> %q", headCursor, next)
	}
}
