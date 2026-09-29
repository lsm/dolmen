package api

import (
	"context"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

type slowFeedEngine struct {
	store.Engine
	delay time.Duration
}

func (e slowFeedEngine) ChangesSince(ctx context.Context, ns, table string, cursor store.Cursor, nsGen [16]byte, scope *store.RowScope, inc store.Incarnation, page store.Page) ([]store.ChangeRecord, store.Cursor, error) {
	select {
	case <-time.After(e.delay):
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	return e.Engine.ChangesSince(ctx, ns, table, cursor, nsGen, scope, inc, page)
}

func TestAWaitThatOutrunsItsReadBudgetIsAnEmptyPageNotAnError(t *testing.T) {
	st := seedWaitForTable(t)
	slow := slowFeedEngine{Engine: st, delay: waitForPollTick + 250*time.Millisecond}

	for _, body := range []string{
		`{"namespace":"cancelns","table":"t","cursor":"begin","timeout_ms":0}`,
		`{"namespace":"cancelns","table":"t","cursor":"begin","timeout_ms":300}`,
	} {
		srv := New(slow, fakeEmb{})
		res, err := srv.Dispatch(context.Background(), "wait_for", []byte(body))
		if err != nil {
			wrapped := WrapError(err)
			t.Fatalf("%s: a wait that outran its own read budget must answer an empty page, not %d %s: %s", body, wrapped.Status, wrapped.Code, wrapped.Message)
		}
		page, ok := res.(map[string]any)
		if !ok {
			t.Fatalf("%s: a wait that read nothing must answer a page, got %T", body, res)
		}
		if changes, _ := page["changes"].([]map[string]any); len(changes) != 0 {
			t.Fatalf("%s: a wait that read nothing must answer an empty page, got %v", body, page["changes"])
		}
		if next, _ := page["next_cursor"].(string); next != "begin" {
			t.Fatalf("%s: the empty page must carry the caller's own cursor so the next wait resumes from it, got %q", body, next)
		}
	}
}
