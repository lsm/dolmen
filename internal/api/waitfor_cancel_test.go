package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type txDoneEngine struct {
	store.Engine
}

func (e txDoneEngine) ChangesSince(ctx context.Context, ns, table string, cursor store.Cursor, nsGen [16]byte, scope *store.RowScope, inc store.Incarnation, page store.Page) ([]store.ChangeRecord, store.Cursor, error) {
	return nil, "", sql.ErrTxDone
}

func waitForOverEngine(t *testing.T, eng store.Engine, ctx context.Context) error {
	t.Helper()
	srv := New(eng, fakeEmb{})
	body, err := json.Marshal(map[string]any{
		"namespace": "cancelns", "table": "t", "timeout_ms": 20000,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Ops["wait_for"].Func(ctx, srv, body)
	return err
}

func seedWaitForTable(t *testing.T) store.Engine {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "cancelns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "cancelns", "t",
		[]schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestACancelledWaitForTeachesCanceledWhateverTheDriverSaid(t *testing.T) {
	eng := seedWaitForTable(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForOverEngine(t, txDoneEngine{Engine: eng}, ctx)
	if err == nil {
		t.Fatal("a cancelled wait_for must fail")
	}
	if code := ops.Classify(err); code != derr.Canceled {
		t.Fatalf("a wait_for cancelled while the driver was rolling its transaction back reported %q (%v); database/sql answers sql.ErrTxDone rather than context.Canceled when it wins that race, and the caller is owed the cancellation either way", code, err)
	}
}

func TestAnUncancelledWaitForStillSurfacesTheDriversError(t *testing.T) {
	eng := seedWaitForTable(t)
	err := waitForOverEngine(t, txDoneEngine{Engine: eng}, context.Background())
	if err == nil {
		t.Fatal("a failing engine must fail the call")
	}
	if code := ops.Classify(err); code == derr.Canceled {
		t.Fatalf("nothing was cancelled, so %v must not be reported as a cancellation: that would hide a real fault behind the caller's own timeout", err)
	}
}
