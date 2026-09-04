package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestMemNamespaceLifecycle(t *testing.T) {
	m := OpenMem()

	if err := m.CreateNamespace("demo"); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if err := m.CreateNamespace("demo"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "namespace demo already exists") {
		t.Fatalf("duplicate create: %v", err)
	}
	// Listing touches nothing: an engine with no namespaces lists empty.
	if nss, err := m.ListNamespaces(); err != nil || len(nss) != 1 || nss[0] != "demo" {
		t.Fatalf("list namespaces: %v %v", nss, err)
	}
	if err := m.DropNamespace("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop missing: %v", err)
	}
	if err := m.DropNamespace("demo"); err != nil {
		t.Fatalf("drop namespace: %v", err)
	}
	if nss, _ := m.ListNamespaces(); len(nss) != 0 {
		t.Fatalf("namespace survived its drop: %v", nss)
	}
}

func TestMemTableRegistry(t *testing.T) {
	m := OpenMem()
	ctx := context.Background()

	sc, err := m.CreateTable(ctx, "demo", "notes", []schema.Field{
		{Name: "title", Type: schema.String, Required: true},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	if sc.Version != 1 || sc.Namespace != "demo" || sc.Name != "notes" {
		t.Fatalf("created schema: %+v", sc)
	}
	// Untyped fields normalize to string, exactly like the SQLite adapter.
	if len(sc.Fields) != 1 || sc.Fields[0].Type != schema.String {
		t.Fatalf("created fields: %+v", sc.Fields)
	}
	if _, err := m.CreateTable(ctx, "demo", "notes", sc.Fields); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate table: %v", err)
	}
	if _, err := m.CreateTable(ctx, "demo", "reserved!", sc.Fields); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid table name: %v", err)
	}
	if _, err := m.CreateTable(ctx, "demo", "other", []schema.Field{
		{Name: "v", Type: schema.Vector, Dim: 4, Default: []any{1, 2}},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid default: %v", err)
	}

	tables, err := m.ListTables(ctx, "demo")
	if err != nil || len(tables) != 1 || tables[0] != "notes" {
		t.Fatalf("list tables: %v %v", tables, err)
	}
	got, count, err := m.DescribeTable(ctx, "demo", "notes")
	if err != nil || got != sc || count != 0 {
		t.Fatalf("describe table: %+v %d %v", got, count, err)
	}
	if _, _, err := m.DescribeTable(ctx, "demo", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("describe missing: %v", err)
	}
	if ms, err := m.ListMigrations(ctx, "demo", "notes"); err != nil || len(ms) != 0 {
		t.Fatalf("list migrations: %v %v", ms, err)
	}

	if err := m.DropTable(ctx, "demo", "notes"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, _, err := m.DescribeTable(ctx, "demo", "notes"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("describe after drop: %v", err)
	}
	if err := m.DropTable(ctx, "demo", "notes"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop missing table: %v", err)
	}
}

// TestMemUnimplementedOps pins the seam stub's refusal contract: every op
// that would need rows, SQL evaluation, or migration execution fails as a
// client-correctable invalid request naming the seam, not as an opaque
// internal error.
func TestMemUnimplementedOps(t *testing.T) {
	m := OpenMem()
	ctx := context.Background()
	if _, err := m.CreateTable(ctx, "demo", "notes", []schema.Field{{Name: "title"}}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	emb := Embedder{Identity: "test"}

	calls := map[string]func() error{
		"insert": func() error {
			_, err := m.Insert(ctx, "demo", "notes", []map[string]any{{"title": "x"}}, emb)
			return err
		},
		"idempotent insert": func() error {
			_, _, err := m.InsertIdempotent(ctx, "demo", "notes", []map[string]any{{"title": "x"}}, emb, "k")
			return err
		},
		"upsert_by_key": func() error {
			_, _, _, err := m.UpsertByKey(ctx, "demo", "notes", []string{"title"}, []map[string]any{{"title": "x"}}, emb)
			return err
		},
		"update": func() error {
			_, err := m.Update(ctx, "demo", "notes", "1=1", nil, map[string]any{"title": "x"}, emb)
			return err
		},
		"upsert": func() error {
			_, err := m.Upsert(ctx, "demo", "notes", "1=1", nil, map[string]any{"title": "x"}, emb)
			return err
		},
		"delete": func() error {
			_, err := m.Delete(ctx, "demo", "notes", "1=1", nil, DeleteOptions{})
			return err
		},
		"query": func() error {
			_, _, err := m.Query(ctx, "demo", "SELECT * FROM notes", nil, 0, 0)
			return err
		},
		"search_fulltext": func() error {
			_, _, err := m.SearchFulltext(ctx, "demo", "notes", "x", 0, 10, false, "", nil)
			return err
		},
		"search_vector": func() error {
			_, err := m.SearchVector(ctx, "demo", "notes", "", []float32{1}, "", 0, 10, false, "", nil, nil)
			return err
		},
		"migrate": func() error {
			_, err := m.Migrate(ctx, "demo", "notes", []schema.Change{{Op: schema.OpSetFulltext, Name: "title", Value: new(bool)}}, emb, 0)
			return err
		},
		"migrate dry-run": func() error {
			_, err := m.PlanMigration(ctx, "demo", "notes", []schema.Change{{Op: schema.OpSetFulltext, Name: "title", Value: new(bool)}}, emb, 0)
			return err
		},
	}
	for op, call := range calls {
		err := call()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", op, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), "invalid request: "+op+" is not implemented by this engine") {
			t.Errorf("%s: refusal does not name the op and the seam: %v", op, err)
		}
	}
}

// TestMemValidateVectorSearch proves the schema-only read validation works
// over the second engine: the same resolveVectorColumn rules the SQLite
// adapter applies, without any SQL.
func TestMemValidateVectorSearch(t *testing.T) {
	m := OpenMem()
	ctx := context.Background()
	if _, err := m.CreateTable(ctx, "demo", "vecs", []schema.Field{
		{Name: "text", Type: schema.Text},
		{Name: "v", Type: schema.Vector, Dim: 4},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Raw-vector query defaults to the first vector column.
	if err := m.ValidateVectorSearch(ctx, "demo", "vecs", "", false, ""); err != nil {
		t.Fatalf("raw query on declared vector column: %v", err)
	}
	// Text queries need a vectorize field; none exists here.
	err := m.ValidateVectorSearch(ctx, "demo", "vecs", "", true, "prov|model")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "text queries") {
		t.Fatalf("text query without vectorize: %v", err)
	}
	// A declared vector column cannot serve a text query either.
	err = m.ValidateVectorSearch(ctx, "demo", "vecs", "v", true, "prov|model")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "cannot target vector column") {
		t.Fatalf("text query on vector column: %v", err)
	}
}

// TestMemConcurrentUse drives registry reads and writes concurrently so the
// race detector covers the snapshot-on-read locking.
func TestMemConcurrentUse(t *testing.T) {
	m := OpenMem()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.CreateNamespace("ns")
			_, _ = m.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "title"}})
			_, _ = m.ListTables(ctx, "ns")
			_, _, _ = m.DescribeTable(ctx, "ns", "notes")
			_ = m.DropNamespace("ns")
		}(i)
	}
	wg.Wait()
}
