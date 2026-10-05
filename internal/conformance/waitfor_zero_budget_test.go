package conformance

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/store"
)

type gatedFeedEngine struct {
	store.Engine
	observed chan context.Context
	release  chan struct{}
}

func (e *gatedFeedEngine) ChangesSince(ctx context.Context, ns, table string, cursor store.Cursor, generation [16]byte, scope *store.RowScope, inc store.Incarnation, page store.Page) ([]store.ChangeRecord, store.Cursor, error) {
	e.observed <- ctx
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	return e.Engine.ChangesSince(ctx, ns, table, cursor, generation, scope, inc, page)
}

func TestZeroTimeoutWaitReadsCommittedChangesUnderOperationBudget(t *testing.T) {
	for _, transport := range []string{"http", "mcp"} {
		for _, cursor := range []string{"", "begin"} {
			t.Run(transport+"/"+cursor, func(t *testing.T) {
				h := newHarnessTimeouts(t, api.Timeouts{Op: time.Minute})
				h.seedTable("poll", "notes", []map[string]any{{"name": "body", "type": "text"}})
				h.mustHTTP("insert", map[string]any{"namespace": "poll", "table": "notes", "records": []map[string]any{{"body": "already committed"}}})
				h.srv.Close()
				gated := &gatedFeedEngine{Engine: h.st, observed: make(chan context.Context, 1), release: make(chan struct{})}
				h.st = gated
				h.serveAPI()
				body := map[string]any{"namespace": "poll", "timeout_ms": 0}
				if cursor != "" {
					body["cursor"] = cursor
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					var data map[string]any
					if transport == "http" {
						status, out := h.httpCall("wait_for", body)
						if status != http.StatusOK {
							t.Errorf("immediate poll: %d %v", status, out)
							return
						}
						data, _ = out["data"].(map[string]any)
					} else {
						result := h.mcpCall("wait_for", body)
						if result.isError() || result.proto != nil {
							t.Errorf("immediate MCP poll: %+v", result)
							return
						}
						data = result.structured()
					}
					changes, _ := data["changes"].([]any)
					if cursor == "begin" && len(changes) != 1 {
						t.Errorf("committed backlog: %v", data)
					}
					if data["next_cursor"] == "" {
						t.Error("poll returned no resumable boundary")
					}
				}()
				ctx := <-gated.observed
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) < 30*time.Second {
					t.Errorf("zero timeout gave the storage read an artificial poll deadline: %v %v; it must inherit the minute operation budget", deadline, ok)
				}
				close(gated.release)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("released poll did not finish")
				}
			})
		}
	}
}
