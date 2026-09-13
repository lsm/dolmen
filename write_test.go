package dolmen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func notesFields() []Field {
	return []Field{
		{Name: "title", Type: String, Fulltext: true},
		{Name: "score", Type: Number},
		{Name: "done", Type: Boolean},
		{Name: "tags", Type: JSON},
	}
}

func openWithNotes(t *testing.T) (*Store, context.Context) {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "app", "notes", notesFields()); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return st, ctx
}

func TestInsertReturnsIdsAndCounts(t *testing.T) {
	st, ctx := openWithNotes(t)
	res, err := st.Insert(ctx, "app", "notes", []map[string]any{
		{"title": "first", "score": 1, "done": false, "tags": []any{"a"}},
		{"title": "second", "score": 2.5, "done": true, "tags": nil},
	}, InsertOptions{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if len(res.Ids) != 2 || res.Inserted != 2 || res.Replayed {
		t.Fatalf("unexpected result %+v", res)
	}
	if res.Changes.Count != 2 || res.Changes.First == 0 || res.Changes.Last == 0 {
		t.Fatalf("write must report its change range, got %+v", res.Changes)
	}
	_, count, err := st.DescribeTable(ctx, "app", "notes")
	if err != nil || count != 2 {
		t.Fatalf("expected 2 rows, got %d (%v)", count, err)
	}
}

func TestInsertIdempotentReplayAndConflict(t *testing.T) {
	st, ctx := openWithNotes(t)
	first, err := st.Insert(ctx, "app", "notes", []map[string]any{{"title": "a"}}, InsertOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	replay, err := st.Insert(ctx, "app", "notes", []map[string]any{{"title": "a"}}, InsertOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed || replay.Inserted != 0 || len(replay.Ids) != 1 || replay.Ids[0] != first.Ids[0] {
		t.Fatalf("replay must return the original ids untouched, got %+v vs %+v", replay, first)
	}
	_, err = st.Insert(ctx, "app", "notes", []map[string]any{{"title": "different"}}, InsertOptions{IdempotencyKey: "k1"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("reusing a key for different records must be a typed conflict, got %v", err)
	}
}

func TestUpsertByKeyConverges(t *testing.T) {
	st, ctx := openWithNotes(t)
	first, err := st.UpsertByKey(ctx, "app", "notes", []string{"title"}, []map[string]any{
		{"title": "doc", "score": 1},
	})
	if err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	if first.Inserted != 1 || first.Updated != 0 {
		t.Fatalf("expected pure insert, got %+v", first)
	}
	second, err := st.UpsertByKey(ctx, "app", "notes", []string{"title"}, []map[string]any{
		{"title": "doc", "score": 9},
	})
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if second.Updated != 1 || second.Inserted != 0 || second.Ids[0] != first.Ids[0] {
		t.Fatalf("expected update in place, got %+v", second)
	}
	_, count, err := st.DescribeTable(ctx, "app", "notes")
	if err != nil || count != 1 {
		t.Fatalf("upsert must converge, got %d rows (%v)", count, err)
	}
}

func TestUpdateMatchesAndCounts(t *testing.T) {
	st, ctx := openWithNotes(t)
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{
		{"title": "a", "score": 1},
		{"title": "b", "score": 2},
	}, InsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := st.Update(ctx, "app", "notes", UpdateOptions{Filter: "score > ?", Args: []any{1}, Set: map[string]any{"done": true}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Updated != 1 || res.Changes.Count != 1 {
		t.Fatalf("expected one update, got %+v", res)
	}
}

func TestDeleteDryRunLimitAndConfirm(t *testing.T) {
	st, ctx := openWithNotes(t)
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{
		{"title": "a"}, {"title": "b"}, {"title": "c"},
	}, InsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	dry, err := st.Delete(ctx, "app", "notes", DeleteOptions{Filter: "title != ?", Args: []any{"a"}, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Matched != 2 || dry.Deleted != 0 {
		t.Fatalf("dry run must count without deleting, got %+v", dry)
	}
	if _, err := st.Delete(ctx, "app", "notes", DeleteOptions{Filter: "title != ?", Args: []any{"a"}, Limit: 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("exceeding the limit without confirm must be rejected, got %v", err)
	}
	res, err := st.Delete(ctx, "app", "notes", DeleteOptions{Filter: "title != ?", Args: []any{"a"}, Limit: 1, Confirm: true})
	if err != nil {
		t.Fatalf("confirmed delete: %v", err)
	}
	if res.Matched != 2 || res.Deleted != 2 {
		t.Fatalf("expected both rows deleted, got %+v", res)
	}
	_, count, err := st.DescribeTable(ctx, "app", "notes")
	if err != nil || count != 1 {
		t.Fatalf("expected 1 remaining row, got %d (%v)", count, err)
	}
}

func TestWriteOperationsAfterCloseReturnErrClosed(t *testing.T) {
	st, ctx := openWithNotes(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{{"title": "x"}}, InsertOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("insert after close must return ErrClosed, got %v", err)
	}
	if _, err := st.Update(ctx, "app", "notes", UpdateOptions{Filter: "1=1", Set: map[string]any{"done": true}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("update after close must return ErrClosed, got %v", err)
	}
	if _, err := st.Delete(ctx, "app", "notes", DeleteOptions{Filter: "1=1"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("delete after close must return ErrClosed, got %v", err)
	}
	if _, err := st.UpsertByKey(ctx, "app", "notes", []string{"title"}, []map[string]any{{"title": "x"}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("upsert after close must return ErrClosed, got %v", err)
	}
}

func TestDeleteRejectsNegativeLimit(t *testing.T) {
	st, ctx := openWithNotes(t)
	if _, err := st.Delete(ctx, "app", "notes", DeleteOptions{Filter: "1=1", Limit: -1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a negative limit must be rejected, not silently defaulted, got %v", err)
	}
}

func TestWriteOperationsHonorCanceledContexts(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{{"body": "x"}}, InsertOptions{}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("insert on a canceled context must classify canceled, got %v", err)
	}
	if _, err := st.UpsertByKey(ctx, "app", "notes", []string{"title"}, []map[string]any{{"title": "x"}}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("upsert on a canceled context must classify canceled, got %v", err)
	}
	if _, err := st.Update(ctx, "app", "notes", UpdateOptions{Filter: "1=1", Set: map[string]any{"done": true}}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("update on a canceled context must classify canceled, got %v", err)
	}
	if _, err := st.Delete(ctx, "app", "notes", DeleteOptions{Filter: "1=1", Confirm: true}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("delete on a canceled context must classify canceled, got %v", err)
	}
	live, err := st.ListNamespaces(context.Background(), ListNamespacesOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("a canceled write must not leave a namespace behind, got %v", live)
	}
}

func TestUpdateCopiesCallerArgs(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "app", "notes", []Field{{Name: "body", Type: Text}, {Name: "score", Type: Number}}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{{"body": "a", "score": 1}, {"body": "b", "score": 2}}, InsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	args := []any{json.Number("1")}
	before := fmt.Sprintf("%v", args)
	if _, err := st.Update(ctx, "app", "notes", UpdateOptions{Filter: "score > ?", Args: args, Set: map[string]any{"score": 9}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if after := fmt.Sprintf("%v", args); after != before {
		t.Fatalf("the caller's args slice must not be mutated: was %s, now %s", before, after)
	}
}
