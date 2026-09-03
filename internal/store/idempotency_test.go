package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestInsertIdempotentReplayReturnsOriginalIDs(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	rec := []map[string]any{{"title": "once", "score": 1}}
	ids1, replayed, err := st.InsertIdempotent(ctx, "test", "notes", rec, testEmbed, "op-123")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if replayed {
		t.Fatal("first insert must not report replayed")
	}

	ids2, replayed, err := st.InsertIdempotent(ctx, "test", "notes", rec, testEmbed, "op-123")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !replayed {
		t.Fatal("retry with a recorded key must report replayed")
	}
	if len(ids2) != 1 || ids2[0] != ids1[0] {
		t.Fatalf("retry must return the original ids, got %v want %v", ids2, ids1)
	}

	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 1 {
		t.Fatalf("retry must not insert another row: %v", rows)
	}
}

func TestInsertIdempotentSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustCreateNotes(t, st)
	ids1, replayed, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "durable", "score": 2}}, testEmbed, "restart-key")
	if err != nil || replayed {
		t.Fatalf("first insert: %v replayed=%v", err, replayed)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	ids2, replayed, err := st2.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "durable", "score": 2}}, testEmbed, "restart-key")
	if err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if !replayed {
		t.Fatal("retry after a process restart must dedupe against the durable key record")
	}
	if len(ids2) != 1 || ids2[0] != ids1[0] {
		t.Fatalf("retry after restart must return the original ids, got %v want %v", ids2, ids1)
	}
	rows, _, err := st2.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 1 {
		t.Fatalf("retry after restart must not insert another row: %v", rows)
	}
}

func TestInsertIdempotentPayloadMismatchRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	if _, _, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "first payload", "score": 1}}, testEmbed, "shared-key"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, replayed, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "different payload", "score": 2}}, testEmbed, "shared-key")
	if err == nil {
		t.Fatal("reusing a key for a different payload must be rejected, not silently replayed")
	}
	if replayed {
		t.Fatal("a rejected mismatch must not report replayed")
	}
	if !strings.Contains(err.Error(), "different insert") {
		t.Fatalf("error should tell the writer keys are single-use, got: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT title FROM notes", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "first payload" {
		t.Fatalf("rejected retry must leave the original row untouched: %v", rows)
	}
}

func TestInsertIdempotentCaseVariantPayloadReplays(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	if _, _, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"Title": "cased", "Score": 1}}, testEmbed, "case-key"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, replayed, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "cased", "score": 1}}, testEmbed, "case-key")
	if err != nil {
		t.Fatalf("case-variant retry: %v", err)
	}
	if !replayed {
		t.Fatal("payloads are hashed after field-name normalization, so a case-variant retry must replay")
	}
}

func TestInsertIdempotentKeysScopedPerTable(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.CreateTable(ctx, "test", "other", noteFields()); err != nil {
		t.Fatalf("create other: %v", err)
	}

	if _, replayed, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "in notes"}}, testEmbed, "k"); err != nil || replayed {
		t.Fatalf("notes insert: %v replayed=%v", err, replayed)
	}
	if _, replayed, err := st.InsertIdempotent(ctx, "test", "other",
		[]map[string]any{{"title": "in other"}}, testEmbed, "k"); err != nil {
		t.Fatalf("other insert with the same key text must be independent: %v", err)
	} else if replayed {
		t.Fatal("the same key text on a different table must not replay")
	}
}

func TestInsertIdempotentKeyValidation(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	if _, _, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "x"}}, testEmbed, ""); err == nil {
		t.Fatal("empty key must be rejected (plain Insert covers that case)")
	}
	if _, _, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "x"}}, testEmbed, strings.Repeat("k", MaxIdempotencyKeyLen+1)); err == nil {
		t.Fatalf("keys longer than %d bytes must be rejected", MaxIdempotencyKeyLen)
	}
	if _, _, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "x"}}, testEmbed, strings.Repeat("k", MaxIdempotencyKeyLen)); err != nil {
		t.Fatalf("keys of exactly %d bytes must be accepted: %v", MaxIdempotencyKeyLen, err)
	}
}

func TestInsertIdempotentReplaySkipsEmbedding(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	calls := 0
	counting := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		return fakeEmbed(ctx, texts)
	}, Identity: "fake-space"}

	if _, _, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "a", "body": "text"}}, counting, "embed-key"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if calls != 1 {
		t.Fatalf("first insert should embed once, got %d calls", calls)
	}
	if _, replayed, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "a", "body": "text"}}, counting, "embed-key"); err != nil || !replayed {
		t.Fatalf("retry: %v replayed=%v", err, replayed)
	}
	if calls != 1 {
		t.Fatalf("replay must not call the embedding provider again, got %d calls", calls)
	}
}

// Two writers racing the same fresh key on one namespace file must converge on
// the winner's ids — never a SQLITE_BUSY_SNAPSHOT error. Each store opens its
// own connection pool, which is the in-process analogue of two server
// processes sharing a WAL database.
func TestInsertIdempotentConcurrentWritersReplay(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st1, err := Open(dir)
	if err != nil {
		t.Fatalf("open st1: %v", err)
	}
	defer st1.Close()
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("open st2: %v", err)
	}
	defer st2.Close()
	mustCreateNotes(t, st1)

	const writers = 8
	rec := []map[string]any{{"title": "raced", "score": 1}}
	type outcome struct {
		id       int64
		replayed bool
		err      error
	}
	out := make([]outcome, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := st1
			if i%2 == 1 {
				st = st2
			}
			<-start
			ids, replayed, err := st.InsertIdempotent(ctx, "test", "notes", rec, testEmbed, "race-key")
			if len(ids) == 1 {
				out[i] = outcome{id: ids[0], replayed: replayed, err: err}
				return
			}
			out[i] = outcome{err: fmt.Errorf("expected one id, got %v", ids)}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, o := range out {
		if o.err != nil {
			t.Fatalf("writer %d failed: %v (a raced retry must replay, not error)", i, o.err)
		}
		if o.id != out[0].id {
			t.Fatalf("writer %d got id %d, writer 0 got %d — all writers must converge on the winner's row", i, o.id, out[0].id)
		}
	}
	inserted := 0
	for _, o := range out {
		if !o.replayed {
			inserted++
		}
	}
	if inserted != 1 {
		t.Fatalf("exactly one writer should insert, got %d", inserted)
	}

	rows, _, err := st1.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 1 {
		t.Fatalf("the race must leave exactly one row: %v", rows)
	}
}
