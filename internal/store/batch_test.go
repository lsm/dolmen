package store

import (
	"context"
	"strings"
	"testing"
)

func openBatchStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateNamespace(context.Background(), "b", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := st.CreateTable(context.Background(), "b", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return st
}

func batchNotes(t *testing.T, st *Store) BatchResult {
	t.Helper()
	res, err := st.Batch(context.Background(), "b", []BatchWrite{
		{Kind: BatchWriteInsert, Table: "notes", Records: []map[string]any{
			{"title": "first", "body": "the dolmen stores stone tables", "score": 5, "done": true, "emb": []any{1.0, 0, 0, 0}},
			{"title": "second", "body": "agents keep their memory here", "score": 3, "emb": []any{0, 1.0, 0, 0}},
		}},
		{Kind: BatchWriteUpdate, Table: "notes", Filter: "done = ?", Args: []any{true}, Set: map[string]any{"score": 9}},
		{Kind: BatchWriteUpsertByKey, Table: "notes", On: []string{"title"}, Records: []map[string]any{
			{"title": "third", "body": "migration and schema evolution", "score": 1, "emb": []any{0.9, 0.1, 0, 0}},
		}},
	}, BatchOpts{}, testEmbed, nil, Incarnation{})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	return res
}

func TestBatchCommitsEveryWriteAndReportsOneResultEach(t *testing.T) {
	st := openBatchStore(t)
	res := batchNotes(t, st)

	if len(res.Results) != 3 {
		t.Fatalf("got %d results, want one per write (3)", len(res.Results))
	}
	if res.Replayed {
		t.Fatalf("a batch with no idempotency key reported a replay")
	}
	if res.Results[0].Inserted != 2 || len(res.Results[0].Ids) != 2 {
		t.Fatalf("insert result = %+v, want 2 ids and inserted 2", res.Results[0])
	}
	if res.Results[1].Updated != 1 {
		t.Fatalf("update result = %+v, want updated 1", res.Results[1])
	}
	if res.Results[2].Inserted != 1 {
		t.Fatalf("upsert_by_key result = %+v, want inserted 1", res.Results[2])
	}
	if res.Changes.Count != 4 {
		t.Fatalf("change range = %+v, want 4 changes (2 insert + 1 update + 1 upsert)", res.Changes)
	}
}

func TestBatchWritesTheWholeFeedInOneCommit(t *testing.T) {
	st := openBatchStore(t)
	before, cursor, err := st.ChangesSince(context.Background(), "b", "", "", [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("changes_since before: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("a fresh namespace reported %d changes", len(before))
	}

	batchNotes(t, st)

	records, _, err := st.ChangesSince(context.Background(), "b", "", cursor, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("changes_since after: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("the batch published %d change records, want all 4 visible after one commit", len(records))
	}
	seen := map[ChangeKind]int{}
	for _, r := range records {
		seen[r.Kind]++
	}
	if seen[ChangeInsert] != 3 || seen[ChangeUpdate] != 1 {
		t.Fatalf("change kinds = %v, want 3 inserts and 1 update", seen)
	}
}

func TestBatchAFailingWriteLeavesNothingBehind(t *testing.T) {
	st := openBatchStore(t)
	ctx := context.Background()

	_, err := st.Batch(ctx, "b", []BatchWrite{
		{Kind: BatchWriteInsert, Table: "notes", Records: []map[string]any{{"title": "kept", "body": "this one is valid", "score": 1, "emb": []any{1.0, 0, 0, 0}}}},
		{Kind: BatchWriteInsert, Table: "notes", Records: []map[string]any{{"titel": "typo", "body": "this one is not", "emb": []any{1.0, 0, 0, 0}}}},
	}, BatchOpts{}, testEmbed, nil, Incarnation{})
	if err == nil {
		t.Fatalf("a batch with an unknown field succeeded, want invalid_request")
	}
	if !strings.Contains(err.Error(), "writes[1]") {
		t.Fatalf("error %q does not name the failing write by index", err)
	}

	res, err := st.Query(ctx, "b", "SELECT count(*) AS n FROM notes", nil, [16]byte{}, Page{})
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["n"] != int64(0) {
		t.Fatalf("the failed batch left %v, want a count of 0", res.Rows)
	}

	records, _, err := st.ChangesSince(ctx, "b", "", "", [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("changes_since: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("the failed batch published %d change records, want none", len(records))
	}
}

func TestBatchReplayReturnsTheStoredResultAndWritesNothing(t *testing.T) {
	st := openBatchStore(t)
	ctx := context.Background()
	writes := []BatchWrite{
		{Kind: BatchWriteInsert, Table: "notes", Records: []map[string]any{{"title": "once", "body": "only ever written once", "score": 2, "emb": []any{1.0, 0, 0, 0}}}},
	}
	opts := BatchOpts{IdempotencyKey: "import-1"}

	first, err := st.Batch(ctx, "b", writes, opts, testEmbed, nil, Incarnation{})
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if first.Replayed {
		t.Fatalf("the first batch reported a replay")
	}
	second, err := st.Batch(ctx, "b", writes, opts, testEmbed, nil, Incarnation{})
	if err != nil {
		t.Fatalf("replayed batch: %v", err)
	}
	if !second.Replayed {
		t.Fatalf("the replay did not report itself as one")
	}
	if len(second.Results) != 1 || len(second.Results[0].Ids) != 1 || second.Results[0].Ids[0] != first.Results[0].Ids[0] {
		t.Fatalf("replay returned %+v, want the stored ids %v", second.Results, first.Results[0].Ids)
	}

	records, _, err := st.ChangesSince(ctx, "b", "", "", [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("changes_since: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("the replay published %d change records, want 1", len(records))
	}
}

func TestBatchRejectsAWriteOverTheRowBudget(t *testing.T) {
	st := openBatchStore(t)
	ctx := context.Background()
	many := make([]map[string]any, 0, MaxRecordsPerInsert+1)
	for i := 0; i <= MaxRecordsPerInsert; i++ {
		many = append(many, map[string]any{"title": "bulk", "body": "over the budget", "score": 1, "emb": []any{1.0, 0, 0, 0}})
	}
	_, err := st.Batch(ctx, "b", []BatchWrite{
		{Kind: BatchWriteInsert, Table: "notes", Records: many},
	}, BatchOpts{}, testEmbed, nil, Incarnation{})
	if err == nil {
		t.Fatalf("a batch over the row budget succeeded, want invalid_request")
	}
	if !strings.Contains(err.Error(), "writes[0]") {
		t.Fatalf("error %q does not name the failing write by index", err)
	}
}
