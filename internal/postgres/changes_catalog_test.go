package postgres

import (
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresChangeLogGainsItsOwnerColumnOnUpgrade(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Type: schema.Text}},
		store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{
		"ALTER TABLE " + s.relation("changes") + " DROP COLUMN owner",
		"DROP INDEX IF EXISTS " + s.relation("changes_owner_feed"),
		"UPDATE " + s.relation("version") + " SET version = 5",
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("shape the change log as version 5 (%s): %v", stmt, err)
		}
	}

	if err := s.bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap over a change log with no owner column: %v", err)
	}

	var version int
	if err := s.pool.QueryRow(ctx, "SELECT version FROM "+s.relation("version")).Scan(&version); err != nil || version != catalogVersion {
		t.Fatalf("catalog version %d after bootstrap, want %d: %v", version, catalogVersion, err)
	}

	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "from alice"}},
		store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("insert after the upgrade: %v", err)
	}
	var owner *string
	if err := s.pool.QueryRow(ctx, "SELECT owner FROM "+s.relation("changes")+" ORDER BY position DESC LIMIT 1").Scan(&owner); err != nil {
		t.Fatalf("read the owner the upgraded change log should carry: %v", err)
	}
	if owner == nil || *owner != "alice" {
		t.Fatalf("a change written after the upgrade carries no owner label, so a scoped feed would treat the writer's own row as foreign: %v", owner)
	}
}

func TestPostgresScopedFeedRefusesRecordsAnOlderProcessWrote(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Type: schema.Text}},
		store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "from alice"}},
		store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("changes")+" SET owner = NULL"); err != nil {
		t.Fatal(err)
	}

	_, _, err := s.ChangesSince(ctx, "app", "notes", store.CursorBegin, [16]byte{},
		&store.RowScope{Owner: "alice"}, store.Incarnation{}, store.Page{Limit: 10})
	if err == nil {
		t.Fatal("a record an older process wrote carries no owner, and a scoped reader must be refused rather than shown a feed with it silently missing")
	}
}

func TestPostgresAScopedFeedRefusesALabelLessRecordWrittenAfterTheUpgrade(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	own := store.WriteOpts{Owner: "alice"}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "hers"}}, own, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	scope := &store.RowScope{Owner: "alice"}
	_, cursor, err := s.ChangesSince(ctx, "app", "notes", store.CursorBegin, [16]byte{}, scope, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatalf("alice could not catch up on her own rows: %v", err)
	}

	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "theirs"}}, store.WriteOpts{Owner: "bob"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("changes")+" SET owner = NULL WHERE namespace='app' AND position = (SELECT max(position) FROM "+s.relation("changes")+" WHERE namespace='app')"); err != nil {
		t.Fatalf("write the record an older process would have left: %v", err)
	}

	_, _, err = s.ChangesSince(ctx, "app", "notes", cursor, [16]byte{}, scope, store.Incarnation{}, store.Page{})
	if !errors.Is(err, store.ErrScopedFeedPredatesLabels) {
		t.Fatalf("a record written without a label after alice caught up answered %v; she must be refused rather than handed a page with rows silently missing", err)
	}
}
