package postgres

import (
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"testing"
)

func TestPostgresCommitCatalogUpgradePreservesLegacyChanges(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	insert := func(body string) {
		t.Helper()
		if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": body}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	insert("legacy")
	for _, stmt := range []string{
		"ALTER TABLE " + s.relation("changes") + " DROP COLUMN IF EXISTS commit_id",
		"ALTER TABLE " + s.relation("namespaces") + " DROP COLUMN IF EXISTS next_commit",
		"UPDATE " + s.relation("version") + " SET version = 8",
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.bootstrap(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	if err := s.pool.QueryRow(ctx, "SELECT version FROM "+s.relation("version")).Scan(&version); err != nil || version != 9 {
		t.Fatalf("version %d, want 9: %v", version, err)
	}
	var legacy *int64
	if err := s.pool.QueryRow(ctx, "SELECT commit_id FROM "+s.relation("changes")+" WHERE namespace='app' AND position=1").Scan(&legacy); err != nil || legacy != nil {
		t.Fatalf("legacy commit %v: %v", legacy, err)
	}
	insert("new")
	var first int64
	if err := s.pool.QueryRow(ctx, "SELECT commit_id FROM "+s.relation("changes")+" WHERE namespace='app' AND position=2").Scan(&first); err != nil || first <= 0 {
		t.Fatalf("new commit %d: %v", first, err)
	}
	if err := s.bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	insert("later")
	var last int64
	if err := s.pool.QueryRow(ctx, "SELECT commit_id FROM "+s.relation("changes")+" WHERE namespace='app' AND position=3").Scan(&last); err != nil || last <= first {
		t.Fatalf("later commit %d <= %d: %v", last, first, err)
	}
}
