package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func seedSearchTable(t *testing.T, s *Store) context.Context {
	t.Helper()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{
		{Name: "title", Fulltext: true},
		{Name: "body", Type: schema.Text, Fulltext: true},
		{Name: "kind"},
	}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	records := []map[string]any{
		{"title": "payment gateway", "body": "the gateway processes a payment refund", "kind": "doc"},
		{"title": "refund policy", "body": "payment terms and conditions", "kind": "policy"},
		{"title": "unrelated notice", "body": "nothing relevant here", "kind": "doc"},
		{"title": "payments", "body": "paying for things", "kind": "doc"},
	}
	if _, err := s.Insert(ctx, "app", "notes", records, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func searchIDs(t *testing.T, result store.SearchResult) []int64 {
	t.Helper()
	ids := make([]int64, len(result.Rows))
	for i, row := range result.Rows {
		id, ok := row["id"].(int64)
		if !ok {
			t.Fatalf("row %d has no int64 id: %+v", i, row)
		}
		ids[i] = id
	}
	return ids
}

func TestPostgresSearchFulltextMatchesAndRanks(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	result, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	ids := searchIDs(t, result)
	if len(ids) != 3 {
		t.Fatalf("payment matched %+v", ids)
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen[1] || !seen[2] || !seen[4] {
		t.Fatalf("stemmed match missed rows: %+v", ids)
	}
	if seen[3] {
		t.Fatalf("unrelated row matched: %+v", ids)
	}
	if result.Truncated {
		t.Fatal("unexpected truncation")
	}
	for _, row := range result.Rows {
		if _, ok := row["_embedding"]; ok {
			t.Fatalf("hidden column exposed: %+v", row)
		}
		if _, ok := row[ftsColumn]; ok {
			t.Fatalf("fts column exposed: %+v", row)
		}
		if row["title"] == nil || row["created_at"] == nil {
			t.Fatalf("row shape: %+v", row)
		}
	}
}

func TestPostgresSearchFulltextGrammar(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	for _, tc := range []struct {
		match string
		want  []int64
	}{
		{"payment gateway", []int64{1}},
		{"payment AND gateway", []int64{1}},
		{"gateway OR policy", []int64{1, 2}},
		{"payment NOT gateway", []int64{2, 4}},
		{`"payment refund"`, []int64{1}},
		{`"refund payment"`, nil},
		{`"processes a payment"`, []int64{1}},
		{"pay*", []int64{1, 2, 4}},
		{"nothing", []int64{3}},
	} {
		result, err := s.SearchFulltext(ctx, "app", "notes", tc.match, "", nil, false, nil, store.Incarnation{}, store.Page{})
		if err != nil {
			t.Fatalf("%q: %v", tc.match, err)
		}
		got := searchIDs(t, result)
		seen := map[int64]bool{}
		for _, id := range got {
			seen[id] = true
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%q matched %+v, want %+v", tc.match, got, tc.want)
		}
		for _, id := range tc.want {
			if !seen[id] {
				t.Fatalf("%q matched %+v, want %+v", tc.match, got, tc.want)
			}
		}
	}
}

func TestPostgresSearchFulltextFilterAndPaging(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	result, err := s.SearchFulltext(ctx, "app", "notes", "payment", "kind = ?", []any{"policy"}, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, result); len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("filtered search: %+v", ids)
	}
	first, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != 1 || !first.Truncated {
		t.Fatalf("first page: rows=%d truncated=%v", len(first.Rows), first.Truncated)
	}
	second, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Rows) != 1 {
		t.Fatalf("second page: %+v", second.Rows)
	}
	if searchIDs(t, first)[0] == searchIDs(t, second)[0] {
		t.Fatal("offset returned the same row")
	}
	last, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 10, Offset: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Rows) != 0 || last.Truncated {
		t.Fatalf("past the end: %+v truncated=%v", last.Rows, last.Truncated)
	}
	if _, err := s.SearchFulltext(ctx, "app", "notes", "payment", "kind = ? AND", []any{"doc"}, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("malformed filter: %v", err)
	}
	if _, err := s.SearchFulltext(ctx, "app", "notes", "payment", "", nil, false, nil, store.Incarnation{}, store.Page{Offset: -1}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("negative offset: %v", err)
	}
}

func TestPostgresSearchFulltextRequiresFulltextField(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "plain", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchFulltext(ctx, "app", "plain", "anything", "", nil, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("table without fulltext fields: %v", err)
	}
}

func TestPostgresSearchFulltextFollowsMigrations(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := seedSearchTable(t, s)
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpSetFulltext, Name: "kind", Value: migrateTrue()},
	}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	result, err := s.SearchFulltext(ctx, "app", "notes", "policy", "", nil, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, result); len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("newly indexed field: %+v", ids)
	}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpDropField, Name: "title"},
	}, store.Embedder{}, store.Incarnation{Version: 2}); err != nil {
		t.Fatal(err)
	}
	result, err = s.SearchFulltext(ctx, "app", "notes", "gateway", "", nil, false, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, result); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("after dropping an indexed field body should still match: %+v", ids)
	}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpSetFulltext, Name: "body", Value: new(bool)},
		{Op: schema.OpSetFulltext, Name: "kind", Value: new(bool)},
	}, store.Embedder{}, store.Incarnation{Version: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchFulltext(ctx, "app", "notes", "gateway", "", nil, false, nil, store.Incarnation{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("search after removing every fulltext field: %v", err)
	}
}
