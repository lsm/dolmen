package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresCursorDurabilityAndRetention(t *testing.T) {
	cfg := testConfig(t)
	retention := time.Hour
	cfg.ChangeRetention = &retention
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "old"}, {"body": "new"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	first, cursor, err := s.ChangesSince(ctx, "app", "", store.CursorBegin, [16]byte{}, nil, store.Incarnation{}, store.Page{Limit: 1})
	if err != nil || len(first) != 1 {
		t.Fatalf("initial replay: %+v %v", first, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, cfg)
	s.now = func() time.Time { return now }
	now = now.Add(50 * time.Minute)
	records, next, err := s.ChangesSince(ctx, "app", "", cursor, [16]byte{}, nil, store.Incarnation{}, store.Page{})
	if err != nil || len(records) != 1 {
		t.Fatalf("restart replay: %+v %v", records, err)
	}
	now = now.Add(50 * time.Minute)
	_, next, err = s.ChangesSince(ctx, "app", "", next, [16]byte{}, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatalf("active chain expired early: %v", err)
	}
	now = now.Add(21 * time.Minute)
	if _, _, err := s.ChangesSince(ctx, "app", "", next, [16]byte{}, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrCursorExpired) {
		t.Fatalf("absolute chain bound ignored: %v", err)
	}
	if _, _, err := s.ChangesSince(ctx, "app", "", "", [16]byte{}, nil, store.Incarnation{}, store.Page{}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+s.relation("changes")).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old changes not pruned: %d %v", count, err)
	}
}

func TestPostgresCursorPinsHistoryAndLifetime(t *testing.T) {
	cfg := testConfig(t)
	retention := time.Hour
	cfg.ChangeRetention = &retention
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body"}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, inc, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "value"}}, store.WriteOpts{}, store.Embedder{}, nil, inc); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	_, token, err := s.ChangesSince(ctx, "app", "notes", store.CursorBegin, inc.NsGen, nil, inc, store.Page{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("changes")+" SET created_at=$1", now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if _, _, err := s.ChangesSince(ctx, "app", "", "", inc.NsGen, nil, store.Incarnation{}, store.Page{}); err != nil {
		t.Fatal(err)
	}
	records, _, err := s.ChangesSince(ctx, "app", "notes", token, inc.NsGen, nil, inc, store.Page{})
	if err != nil || len(records) != 1 {
		t.Fatalf("active chain lost history: %+v %v", records, err)
	}
	if err := s.DropTable(ctx, "app", "notes", inc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, inc.NsGen); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "successor"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	successor, _, err := s.ChangesSince(ctx, "app", "notes", token, inc.NsGen, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatalf("pre-drop token on the successor feed: %v", err)
	}
	if len(successor) != 1 || successor[0].Kind != store.ChangeInsert {
		t.Fatalf("successor feed = %+v, want only the successor's own event", successor)
	}
	if err := s.DropNamespace(ctx, "app", inc.NsGen); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ChangesSince(ctx, "app", "", token, [16]byte{}, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrCursorExpired) {
		t.Fatalf("namespace replacement reused token: %v", err)
	}
}

func TestPostgresNamespaceGoneDetectsDropAndReplacement(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	var live [16]byte
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		live = n.generation
		if s.namespaceGone(ctx, tx, "app", live) {
			t.Fatal("a live namespace must not read as gone")
		}
		if !s.namespaceGone(ctx, tx, "app", [16]byte{9}) {
			t.Fatal("a different generation must read as gone")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, "app", live); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		if !s.namespaceGone(ctx, tx, "app", live) {
			t.Fatal("a recreated namespace must read as gone for the predecessor's generation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
}
