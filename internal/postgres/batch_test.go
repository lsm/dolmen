package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

var batchEmbed = store.Embedder{Identity: "pg-batch"}

func openBatchStore(t *testing.T) *Store {
	t.Helper()
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "b", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{
		{Name: "title"},
		{Name: "body", Type: schema.Text},
		{Name: "score", Type: schema.Number},
		{Name: "done", Type: schema.Boolean},
	}
	if _, err := s.CreateTable(ctx, "b", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	return s
}

func batchNotes(ctx context.Context, s *Store) []store.BatchWrite {
	return []store.BatchWrite{
		{Kind: store.BatchWriteInsert, Table: "notes", Records: []map[string]any{
			{"title": "first", "body": "a body", "score": 5, "done": true},
			{"title": "second", "body": "a body", "score": 3},
		}},
		{Kind: store.BatchWriteUpdate, Table: "notes", Filter: "done = ?", Args: []any{true}, Set: map[string]any{"score": 9}},
	}
}

func TestPostgresBatchCommitsEveryWriteAsOneFeed(t *testing.T) {
	s := openBatchStore(t)
	ctx := t.Context()
	_, head, err := s.ChangesSince(ctx, "b", "", store.CursorBegin, [16]byte{}, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Batch(ctx, "b", batchNotes(ctx, s), store.BatchOpts{}, batchEmbed, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("got %d results, want one per write", len(res.Results))
	}
	if res.Results[0].Inserted != 2 || len(res.Results[0].Ids) != 2 {
		t.Fatalf("insert result = %+v, want 2 ids and inserted 2", res.Results[0])
	}
	if res.Results[1].Updated != 1 {
		t.Fatalf("update result = %+v, want updated 1", res.Results[1])
	}
	records, _, err := s.ChangesSince(ctx, "b", "", head, [16]byte{}, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("the batch published %d change records, want 3 (2 insert + 1 update) in one commit", len(records))
	}
}

func TestPostgresBatchFailingWriteLeavesNothing(t *testing.T) {
	s := openBatchStore(t)
	ctx := t.Context()
	_, err := s.Batch(ctx, "b", []store.BatchWrite{
		{Kind: store.BatchWriteInsert, Table: "notes", Records: []map[string]any{{"title": "kept", "body": "a body", "score": 1}}},
		{Kind: store.BatchWriteInsert, Table: "notes", Records: []map[string]any{{"titel": "typo", "body": "a body"}}},
	}, store.BatchOpts{}, batchEmbed, nil, store.Incarnation{})
	if err == nil {
		t.Fatal("a batch with an unknown field succeeded, want invalid_request")
	}
	if !strings.Contains(err.Error(), "writes[1]") {
		t.Fatalf("error %q does not name the failing write by index", err)
	}
	rows, err := s.Query(ctx, "b", "SELECT count(*) AS n FROM notes", nil, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 || rows.Rows[0]["n"] != int64(0) {
		t.Fatalf("the failed batch left %v, want a count of 0", rows.Rows)
	}
}

func TestPostgresBatchReplaysAndRetiresTheKeyOnDrop(t *testing.T) {
	s := openBatchStore(t)
	ctx := t.Context()
	writes := batchNotes(ctx, s)
	opts := store.BatchOpts{IdempotencyKey: "pg-1"}

	_, head, err := s.ChangesSince(ctx, "b", "", store.CursorBegin, [16]byte{}, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Batch(ctx, "b", writes, opts, batchEmbed, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed {
		t.Fatal("the first batch reported a replay")
	}

	second, err := s.Batch(ctx, "b", writes, opts, batchEmbed, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed {
		t.Fatal("the key did not replay while its tables existed")
	}
	if len(second.Results) != 2 || second.Results[0].Ids[0] != first.Results[0].Ids[0] {
		t.Fatalf("replay returned %+v, want the stored ids %v", second.Results, first.Results[0].Ids)
	}
	records, _, err := s.ChangesSince(ctx, "b", "", head, [16]byte{}, nil, store.Incarnation{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("the batch and its replay published %d records together, want 3", len(records))
	}

	if err := s.DropTable(ctx, "b", "notes", store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "b", "notes", []schema.Field{{Name: "title"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Batch(ctx, "b", writes, opts, batchEmbed, nil, store.Incarnation{})
	if err == nil {
		t.Fatalf("the key still replayed after one of its tables was dropped: %+v", after)
	}
	if after.Replayed {
		t.Fatal("the key reported a replay for a batch whose record should have been dropped")
	}
}

func TestPostgresBatchRejectsAnEmptyWriteList(t *testing.T) {
	s := openBatchStore(t)
	ctx := t.Context()
	if _, err := s.Batch(ctx, "b", nil, store.BatchOpts{}, batchEmbed, nil, store.Incarnation{}); err == nil {
		t.Fatal("an empty batch succeeded, want invalid_request")
	}
}
