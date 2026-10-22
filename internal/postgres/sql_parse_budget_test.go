package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

func TestCompilingConfinedSQLStopsAtTheCallersDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := compileSQL(ctx, "SELECT * FROM notes", 0, "namespace_schema", queryTestTables()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller that has already gone must be reported, not parsed through: %v", err)
	}
	if _, _, err := compileFilterSQL(ctx, "1=1", 0, "namespace_schema", queryTestTables()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a mutation filter must stop at the caller's deadline too: %v", err)
	}

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if _, _, err := compileSQL(expired, "SELECT * FROM notes", 0, "namespace_schema", queryTestTables()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an expired deadline must be reported, not parsed through: %v", err)
	}
}

func TestTheConfinedSQLParserIsReadyByTheTimeAStoreIsOpen(t *testing.T) {
	st := openTest(t, testConfig(t))
	if err := st.CreateNamespace(t.Context(), "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	start := time.Now()
	if _, err := st.Query(t.Context(), "test", "SELECT 1 AS one", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatalf("query: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the first confined query took %v: the SQL parser's one-time start-up belongs to opening the store, not to a caller's operation", took)
	}
}
