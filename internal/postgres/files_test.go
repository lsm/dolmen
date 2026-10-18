package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
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

func TestPostgresSetFileConstraintsChecksStoredReferences(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "fil", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "fil", "docs", []schema.Field{{Name: "doc", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	refs := []map[string]any{
		{"doc": `{"sha256":"` + strings.Repeat("a", 64) + `","size":10,"media_type":"text/plain","name":null}`},
		{"doc": `{"sha256":"` + strings.Repeat("b", 64) + `","size":5000,"media_type":"application/pdf","name":"big.pdf"}`},
		{"doc": nil},
	}
	if _, err := s.Insert(ctx, "fil", "docs", refs, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	plan := func(ch schema.Change, scope *store.RowScope) error {
		return s.write(ctx, "fil", [16]byte{}, func(tx pgx.Tx, n namespace) error {
			state, err := s.loadTable(ctx, tx, n, "docs")
			if err != nil {
				return err
			}
			state.schema.Fields[0].Type = schema.File
			_, err = s.planMigration(ctx, tx, n, state, []schema.Change{ch}, store.Embedder{}, 0, scope)
			return err
		})
	}
	limit := int64(1024)
	err := plan(schema.Change{Op: schema.OpSetFileConstraints, Name: "doc", MaxBytes: &limit}, nil)
	if err == nil || !strings.Contains(err.Error(), "1 rows hold files that break them (ids 2)") {
		t.Fatalf("a max_bytes below a stored file must name the violating row, got %v", err)
	}
	pdfOnly := []string{"application/pdf"}
	if err := plan(schema.Change{Op: schema.OpSetFileConstraints, Name: "doc", MediaTypes: &pdfOnly}, &store.RowScope{}); err == nil || strings.Contains(err.Error(), "ids") {
		t.Fatalf("a row-scoped caller must learn neither the count nor the ids, got %v", err)
	}
	mixed := []string{"text/*", "application/pdf"}
	if err := plan(schema.Change{Op: schema.OpSetFileConstraints, Name: "doc", MediaTypes: &mixed}, nil); err != nil {
		t.Fatalf("constraints every stored file meets must plan: %v", err)
	}
}

func TestPostgresSetFileConstraintsRefusesAChangeWithNothingToSet(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "fil", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "fil", "docs", []schema.Field{{Name: "doc", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	limit := int64(10)
	for name, c := range map[string]struct {
		ch   schema.Change
		want string
	}{
		"nothing to set":       {schema.Change{Op: schema.OpSetFileConstraints, Name: "doc"}, "needs max_bytes"},
		"max_bytes on another": {schema.Change{Op: schema.OpSetEnum, Name: "doc", Enum: &[]string{}, MaxBytes: &limit}, "only allowed on set_file_constraints"},
	} {
		err := s.write(ctx, "fil", [16]byte{}, func(tx pgx.Tx, n namespace) error {
			state, err := s.loadTable(ctx, tx, n, "docs")
			if err != nil {
				return err
			}
			_, err = s.planMigration(ctx, tx, n, state, []schema.Change{c.ch}, store.Embedder{}, 0, nil)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", name, err, c.want)
		}
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

func TestPostgresRefusesFileFieldsWithoutASharedFileStore(t *testing.T) {
	cfg := testConfig(t)
	for _, shared := range []bool{false, true} {
		cfg.SharedFiles = shared
		s := openTest(t, cfg)
		ctx := t.Context()
		ns := "local"
		if shared {
			ns = "shared"
		}
		if err := s.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		_, err := s.CreateTable(ctx, ns, "docs", []schema.Field{{Name: "doc", Type: schema.File}}, store.TableOpts{}, [16]byte{})
		if shared && err != nil {
			t.Fatalf("with a shared file store, PostgreSQL must accept a file field: %v", err)
		}
		if !shared && (err == nil || !strings.Contains(err.Error(), "-files-store s3")) {
			t.Fatalf("without a shared file store, PostgreSQL must refuse a file field naming -files-store s3, got %v", err)
		}
	}
}
