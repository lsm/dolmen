package dolmen

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func batchFixture(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateTable(context.Background(), "App", "Notes", []Field{
		{Name: "title", Type: String},
		{Name: "body", Type: Text},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	return st
}

func batchRowCount(t *testing.T, st *Store) int {
	t.Helper()
	res, err := st.Query(context.Background(), "app", "SELECT * FROM notes", QueryOptions{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return len(res.Rows)
}

func TestBatchAppliesEveryWriteInOrder(t *testing.T) {
	st := batchFixture(t)
	ctx := context.Background()

	res, err := st.Batch(ctx, "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{
			{"title": "a", "body": "one"},
			{"title": "b", "body": "two"},
		}},
		{Kind: BatchUpdate, Table: "notes", Filter: "title = $1", Args: []any{"a"}, Set: map[string]any{"body": "edited"}},
	}, BatchOptions{})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(res.Results))
	}
	if res.Replayed {
		t.Error("a first batch is not a replay")
	}
	if res.Results[0].Kind != BatchInsert || res.Results[0].Inserted != 2 || len(res.Results[0].Ids) != 2 {
		t.Fatalf("insert result = %+v, want 2 inserted with 2 ids", res.Results[0])
	}
	if res.Results[1].Kind != BatchUpdate || res.Results[1].Updated != 1 {
		t.Fatalf("update result = %+v, want 1 updated", res.Results[1])
	}
}

func TestBatchIsAllOrNothing(t *testing.T) {
	st := batchFixture(t)

	_, err := st.Batch(context.Background(), "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{{"title": "kept", "body": "x"}}},
		{Kind: BatchInsert, Table: "missing", Records: []map[string]any{{"title": "no", "body": "y"}}},
	}, BatchOptions{})
	if err == nil {
		t.Fatal("a write against a missing table must fail the batch")
	}
	if n := batchRowCount(t, st); n != 0 {
		t.Fatalf("a failed batch left %d rows behind, want 0", n)
	}
}

func TestBatchKeepsTheErrorClassOfTheFailingWrite(t *testing.T) {
	st := batchFixture(t)

	_, err := st.Batch(context.Background(), "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{{"title": "x", "body": "y"}}},
		{Kind: BatchInsert, Table: "missing", Records: []map[string]any{{"title": "a", "body": "b"}}},
	}, BatchOptions{})
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("error %v is neither invalid_request nor not_found, so the class is flattened", err)
	}
}

func TestBatchReplaysAnIdenticalRequest(t *testing.T) {
	st := batchFixture(t)
	ctx := context.Background()
	writes := []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{{"title": "a", "body": "one"}}},
	}
	opts := BatchOptions{IdempotencyKey: "k1"}

	first, err := st.Batch(ctx, "app", writes, opts)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Replayed {
		t.Error("the first call is not a replay")
	}
	second, err := st.Batch(ctx, "app", writes, opts)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Replayed {
		t.Fatal("re-sending the identical body with the same key must replay")
	}
	if len(second.Results) != 1 || len(second.Results[0].Ids) != len(first.Results[0].Ids) {
		t.Fatalf("replay results = %+v, want the first call's ids %v", second.Results, first.Results[0].Ids)
	}
	if n := batchRowCount(t, st); n != 1 {
		t.Fatalf("a replay wrote again: %d rows, want 1", n)
	}
}

func TestBatchConflictsOnTheSameKeyWithADifferentBody(t *testing.T) {
	st := batchFixture(t)
	ctx := context.Background()

	if _, err := st.Batch(ctx, "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{{"title": "a", "body": "one"}}},
	}, BatchOptions{IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := st.Batch(ctx, "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{{"title": "different", "body": "two"}}},
	}, BatchOptions{IdempotencyKey: "k1"})
	if err == nil {
		t.Fatal("the same key with a different body must conflict")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error %v is not a conflict", err)
	}
}

func TestBatchOptionsChangeWhatItDoesAndAreHashed(t *testing.T) {
	st := batchFixture(t)
	ctx := context.Background()
	if _, err := st.Batch(ctx, "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{
			{"title": "a", "body": "1"}, {"title": "b", "body": "2"}, {"title": "c", "body": "3"},
		}},
	}, BatchOptions{IdempotencyKey: "seed"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	writes := []BatchWrite{{Kind: BatchDelete, Table: "notes", Filter: "1=1"}}
	if _, err := st.Batch(ctx, "app", writes, BatchOptions{IdempotencyKey: "cap", Limit: 2}); err == nil {
		t.Fatal("a delete matching more rows than the explicit cap, without confirm, must be refused")
	}
	if n := batchRowCount(t, st); n != 3 {
		t.Fatalf("a refused batch changed the table: %d rows, want 3", n)
	}
	if _, err := st.Batch(ctx, "app", writes, BatchOptions{IdempotencyKey: "cap", Limit: 2, Confirm: true}); err != nil {
		t.Fatalf("the same delete with confirm: %v", err)
	}
	if n := batchRowCount(t, st); n != 0 {
		t.Fatalf("got %d rows, want the delete to have removed all 3", n)
	}

	if _, err := st.Batch(ctx, "app", writes, BatchOptions{IdempotencyKey: "cap", Limit: 3}); err == nil {
		t.Fatal("the same key with a body that differs only in limit and confirm must conflict, not replay")
	}
}

func TestBatchRefusesAnEmptyWriteList(t *testing.T) {
	st := batchFixture(t)
	if _, err := st.Batch(context.Background(), "app", nil, BatchOptions{}); err == nil {
		t.Fatal("an empty batch must be refused")
	}
}

func TestBatchNormalisesTheNamespaceAndTheTable(t *testing.T) {
	st := batchFixture(t)
	ctx := context.Background()
	if _, err := st.Batch(ctx, "App", []BatchWrite{
		{Kind: BatchInsert, Table: "Notes", Records: []map[string]any{{"title": "a", "body": "b"}}},
	}, BatchOptions{}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if n := batchRowCount(t, st); n != 1 {
		t.Fatalf("got %d rows, want 1", n)
	}
}

func TestBatchRechecksTheEmbeddingSpace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	emb, err := Open(dir, WithEmbedding(&staticProvider{identity: "fake|v1"}))
	if err != nil {
		t.Fatalf("open with provider: %v", err)
	}
	if _, err := emb.CreateTable(ctx, "app", "notes", []Field{
		{Name: "body", Type: Text, Vectorize: true},
	}); err != nil {
		emb.Close()
		t.Fatalf("create: %v", err)
	}
	if _, err := emb.Batch(ctx, "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{{"body": "pinned"}}},
	}, BatchOptions{}); err != nil {
		emb.Close()
		t.Fatalf("first batch pins the space: %v", err)
	}
	if err := emb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	other, err := Open(dir, WithEmbedding(&staticProvider{identity: "other|v1"}))
	if err != nil {
		t.Fatalf("reopen with a different identity: %v", err)
	}
	defer other.Close()
	_, err = other.Batch(ctx, "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{{"body": "foreign"}}},
	}, BatchOptions{})
	if err == nil {
		t.Fatal("a batch whose identity differs from the table's embed_space must be refused")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error %v is not invalid_request", err)
	}
}

func TestBatchRejectsANegativeLimit(t *testing.T) {
	st := batchFixture(t)
	_, err := st.Batch(context.Background(), "app", []BatchWrite{
		{Kind: BatchDelete, Table: "notes", Filter: "1=1"},
	}, BatchOptions{Limit: -3})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error %v is not invalid_request, so a negative limit runs as the default cap", err)
	}
}

func TestBatchDoesNotMutateTheCallersArgs(t *testing.T) {
	st := batchFixture(t)
	ctx := context.Background()
	if _, err := st.Batch(ctx, "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{{"title": "a", "body": "one"}}},
	}, BatchOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	args := []any{json.Number("5.0")}
	writes := []BatchWrite{
		{Kind: BatchUpdate, Table: "notes", Filter: "title = $1", Args: args, Set: map[string]any{"body": "first"}},
	}
	if _, err := st.Batch(ctx, "app", writes, BatchOptions{IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(args) != 1 {
		t.Fatalf("the caller's args slice was resized to %d, want 1", len(args))
	}
	if _, ok := args[0].(json.Number); !ok {
		t.Fatalf("the caller's arg 0 is %T, want the json.Number it was given", args[0])
	}

	again, err := st.Batch(ctx, "app", writes, BatchOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("replay with the caller's own slice must replay, not conflict: %v", err)
	}
	if !again.Replayed {
		t.Fatal("the second call did not replay, so the args were rewritten and the body no longer matches")
	}
}

func TestBatchCommitsTheFeedOnce(t *testing.T) {
	st := batchFixture(t)
	res, err := st.Batch(context.Background(), "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{
			{"title": "a", "body": "1"}, {"title": "b", "body": "2"},
		}},
		{Kind: BatchUpdate, Table: "notes", Filter: "title = $1", Args: []any{"a"}, Set: map[string]any{"body": "edited"}},
	}, BatchOptions{})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if res.Changes.Count != 3 {
		t.Fatalf("the batch reports %d changed rows, want 3 - a batch commits the feed once", res.Changes.Count)
	}
	if res.Changes.First == 0 || res.Changes.Last != res.Changes.First+res.Changes.Count-1 {
		t.Fatalf("change range %+v is not a contiguous run of %d", res.Changes, res.Changes.Count)
	}
	if got := res.Results[0].Changes.Count; got != 2 {
		t.Fatalf("the insert reports %d changed rows, want 2", got)
	}
	if got := res.Results[1].Changes.Count; got != 1 {
		t.Fatalf("the update reports %d changed rows, want 1", got)
	}
}

func TestBatchDoesNotMutateTheCallersRecords(t *testing.T) {
	st := batchFixture(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "app", "keyed", []Field{
		{Name: "k", Type: String},
		{Name: "body", Type: Text, Default: "filled-in"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	rec := map[string]any{"k": "one"}
	writes := []BatchWrite{{
		Kind:    BatchUpsertByKey,
		Table:   "keyed",
		On:      []string{"k"},
		Records: []map[string]any{rec},
	}}
	if _, err := st.Batch(ctx, "app", writes, BatchOptions{IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(rec) != 1 {
		t.Fatalf("the caller's record grew to %d fields, want 1: %v", len(rec), rec)
	}
	again, err := st.Batch(ctx, "app", writes, BatchOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("replay with the caller's own records must replay, not conflict: %v", err)
	}
	if !again.Replayed {
		t.Fatal("the second call did not replay, so the records were rewritten and the body no longer matches")
	}
}

func TestBatchNamesTheFailingWriteByIndex(t *testing.T) {
	st := batchFixture(t)
	_, err := st.Batch(context.Background(), "app", []BatchWrite{
		{Kind: BatchInsert, Table: "notes", Records: []map[string]any{{"title": "a", "body": "b"}}},
		{Kind: BatchInsert, Table: "missing", Records: []map[string]any{{"title": "c", "body": "d"}}},
	}, BatchOptions{})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "writes[1]") {
		t.Fatalf("error %q does not name the failing write by index", err.Error())
	}
}
