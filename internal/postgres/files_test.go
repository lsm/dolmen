package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lsm/dolmen/internal/schema"
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

type confirmProbe struct {
	s      *Store
	ns     string
	calls  int
	locked error
}

func (p *confirmProbe) Attach(context.Context, string, string) (store.FileBlob, error) {
	return store.FileBlob{}, errors.New("unused")
}
func (p *confirmProbe) Describe(string) (store.FileBlob, error) {
	return store.FileBlob{}, errors.New("unused")
}
func (p *confirmProbe) Pin(string, string) error { return errors.New("unused") }
func (p *confirmProbe) Fetch(context.Context, string) (string, error) {
	return "", errors.New("unused")
}

func (p *confirmProbe) Confirm(ctx context.Context, ns string) error {
	p.calls++
	_, p.locked = p.s.pool.Exec(ctx, "SELECT 1 FROM "+p.s.relation("namespaces")+" WHERE name = $1 FOR NO KEY UPDATE NOWAIT", p.ns)
	return nil
}

func TestPostgresWritesConfirmTheirFilesHoldingTheNamespaceLock(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "fil", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "fil", "docs", []schema.Field{{Name: "title", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	probe := &confirmProbe{s: s, ns: "fil"}
	if _, err := s.Insert(store.WithFileSource(ctx, probe), "fil", "docs", []map[string]any{{"title": "a"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 1 {
		t.Fatalf("a write must confirm its files once before committing, got %d calls", probe.calls)
	}
	var pgErr *pgconn.PgError
	if !errors.As(probe.locked, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("the confirm must run while the write holds the namespace lock; got %v", probe.locked)
	}
}
