package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lsm/dolmen/internal/store"
)

type lockProbe struct {
	s      *Store
	ns     string
	calls  int
	locked error
	refs   map[string]bool
}

func (p *lockProbe) Sweep(ctx context.Context, ns string, refs map[string]bool) (store.FileSweep, error) {
	p.calls++
	p.refs = refs
	_, err := p.s.pool.Exec(ctx, "SELECT 1 FROM "+p.s.relation("namespaces")+" WHERE name = $1 FOR NO KEY UPDATE NOWAIT", p.ns)
	p.locked = err
	return store.FileSweep{}, nil
}

func TestPostgresVacuumSweepsFilesHoldingTheNamespaceWriteLock(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "fil", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	probe := &lockProbe{s: s, ns: "fil"}
	res, err := s.Vacuum(store.WithFileSweeper(ctx, probe), "fil")
	if err != nil {
		t.Fatal(err)
	}
	if probe.calls != 1 || res.Files == nil {
		t.Fatalf("vacuum must sweep the namespace's files once and report it: calls=%d files=%v", probe.calls, res.Files)
	}
	var pgErr *pgconn.PgError
	if !errors.As(probe.locked, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("while the sweep runs, a write must not be able to take the namespace's lock; got %v", probe.locked)
	}
	if len(probe.refs) != 0 {
		t.Fatalf("a namespace without file fields references no files, got %v", probe.refs)
	}
	if res, err := s.Vacuum(ctx, "fil"); err != nil || res.Files != nil {
		t.Fatalf("vacuum without a file store reports no sweep: %v %v", res.Files, err)
	}
}
