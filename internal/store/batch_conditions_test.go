package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func expectedBatchRows(t *testing.T, w BatchWrite, n int64) BatchWrite {
	t.Helper()
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"ExpectedRows":%d}`, n)), &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func batchConditionRun(t *testing.T, s *Store, writes []BatchWrite, opts BatchOpts) (BatchResult, error) {
	t.Helper()
	return s.Batch(t.Context(), "b", writes, opts, Embedder{}, nil, Incarnation{})
}

func batchConditionCount(t *testing.T, s *Store, filter string) int64 {
	t.Helper()
	r, err := s.Query(t.Context(), "b", "SELECT count(*) AS n FROM notes WHERE "+filter, nil, [16]byte{}, Page{})
	if err != nil {
		t.Fatal(err)
	}
	return r.Rows[0]["n"].(int64)
}

func batchConditionSeed(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Insert(t.Context(), "b", "notes", []map[string]any{{"title": "job", "score": 7}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
}

func TestBatchExpectedRowsRollsBackPriorWritesFeedAndKey(t *testing.T) {
	for _, kind := range []BatchWriteKind{BatchWriteUpdate, BatchWriteDelete} {
		t.Run(string(kind), func(t *testing.T) {
			s := openBatchStore(t)
			w := BatchWrite{Kind: kind, Table: "notes", Filter: "title = 'missing'"}
			if kind == BatchWriteUpdate {
				w.Set = map[string]any{"score": 8}
			}
			writes := []BatchWrite{{Kind: BatchWriteInsert, Table: "notes", Records: []map[string]any{{"title": "event", "score": 7}}}, expectedBatchRows(t, w, 1)}
			opts := BatchOpts{IdempotencyKey: "claim"}
			r, err := batchConditionRun(t, s, writes, opts)
			if !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "writes[1]: expected_rows mismatch") || len(r.Results) != 0 {
				t.Fatalf("stale condition = %+v %v, want indexed conflict and no partial result", r, err)
			}
			if n := batchConditionCount(t, s, "1=1"); n != 0 {
				t.Fatalf("rollback left %d rows", n)
			}
			feed, _, err := s.ChangesSince(t.Context(), "b", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
			if err != nil || len(feed) != 0 {
				t.Fatalf("rollback feed = %+v %v", feed, err)
			}
			writes[1].Filter = "title = 'event'"
			r, err = batchConditionRun(t, s, writes, opts)
			if err != nil || r.Replayed {
				t.Fatalf("failed condition consumed the key: %+v %v", r, err)
			}
		})
	}
}

func TestBatchExpectedRowsZeroOmissionAndUnchangedValues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     BatchWriteKind
		filter   string
		expect   *int64
		conflict bool
	}{
		{"omitted", BatchWriteUpdate, "title = 'missing'", nil, false},
		{"zero missing", BatchWriteUpdate, "title = 'missing'", new(int64), false},
		{"zero existing", BatchWriteUpdate, "title = 'job'", new(int64), true},
		{"delete zero existing", BatchWriteDelete, "title = 'job'", new(int64), true},
		{"one missing", BatchWriteUpdate, "title = 'missing'", func() *int64 { n := int64(1); return &n }(), true},
		{"no-op matches one", BatchWriteUpdate, "title = 'job'", func() *int64 { n := int64(1); return &n }(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openBatchStore(t)
			batchConditionSeed(t, s)
			w := BatchWrite{Kind: tc.kind, Table: "notes", Filter: tc.filter}
			if tc.kind == BatchWriteUpdate {
				w.Set = map[string]any{"score": 7}
			}
			if tc.expect != nil {
				w = expectedBatchRows(t, w, *tc.expect)
			}
			r, err := batchConditionRun(t, s, []BatchWrite{w}, BatchOpts{})
			if tc.conflict {
				if !errors.Is(err, derr.ErrConflict) {
					t.Fatalf("want conflict, got %+v %v", r, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if n := batchConditionCount(t, s, "title = 'job' AND score = 7"); n != 1 {
				t.Fatalf("original row lost: %d", n)
			}
			if tc.name == "no-op matches one" && r.Results[0].Updated != 1 {
				t.Fatalf("unchanged row must match: %+v", r)
			}
		})
	}
}

func TestBatchExpectedRowsObservesEarlierWrites(t *testing.T) {
	s := openBatchStore(t)
	r, err := batchConditionRun(t, s, []BatchWrite{
		{Kind: BatchWriteInsert, Table: "notes", Records: []map[string]any{{"title": "job", "score": 7}}},
		expectedBatchRows(t, BatchWrite{Kind: BatchWriteUpdate, Table: "notes", Filter: "score = 7", Set: map[string]any{"score": 8}}, 1),
		expectedBatchRows(t, BatchWrite{Kind: BatchWriteDelete, Table: "notes", Filter: "score = 8"}, 1),
	}, BatchOpts{})
	if err != nil || len(r.Results) != 3 || r.Results[1].Updated != 1 || r.Results[2].Matched != 1 || r.Changes.Count != 3 {
		t.Fatalf("ordered batch = %+v %v", r, err)
	}
	if n := batchConditionCount(t, s, "1=1"); n != 0 {
		t.Fatalf("ordered delete left %d rows", n)
	}
}

func TestBatchExpectedRowsReplaysBeforeRechecking(t *testing.T) {
	s := openBatchStore(t)
	batchConditionSeed(t, s)
	w := expectedBatchRows(t, BatchWrite{Kind: BatchWriteUpdate, Table: "notes", Filter: "score = 7", Set: map[string]any{"score": 8}}, 1)
	opts := BatchOpts{IdempotencyKey: "version-seven"}
	first, err := batchConditionRun(t, s, []BatchWrite{w}, opts)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := batchConditionRun(t, s, []BatchWrite{w}, opts)
	if err != nil || !replay.Replayed || replay.Changes != first.Changes {
		t.Fatalf("exact replay = %+v %v", replay, err)
	}
	w = expectedBatchRows(t, w, 0)
	if _, err := batchConditionRun(t, s, []BatchWrite{w}, opts); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("changed expectation under same key = %v", err)
	}
}

func TestBatchExpectedRowsValidation(t *testing.T) {
	for _, tc := range []struct {
		kind BatchWriteKind
		n    int64
	}{{BatchWriteUpdate, -1}, {BatchWriteDelete, 1001}, {BatchWriteInsert, 1}, {BatchWriteUpsert, 1}, {BatchWriteUpsertByKey, 1}} {
		t.Run(fmt.Sprintf("%s/%d", tc.kind, tc.n), func(t *testing.T) {
			s := openBatchStore(t)
			w := expectedBatchRows(t, BatchWrite{Kind: tc.kind, Table: "notes", Filter: "1=1", Set: map[string]any{"score": 8}, On: []string{"title"}, Records: []map[string]any{{"title": "x"}}}, tc.n)
			_, err := batchConditionRun(t, s, []BatchWrite{w}, BatchOpts{})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "writes[0]") || !strings.Contains(err.Error(), "expected_rows") {
				t.Fatalf("invalid expectation = %v", err)
			}
			if n := batchConditionCount(t, s, "1=1"); n != 0 {
				t.Fatalf("invalid input wrote %d rows", n)
			}
		})
	}
}

func TestBatchExpectedRowsConcurrentClaimHasOneEvent(t *testing.T) {
	s := openBatchStore(t)
	batchConditionSeed(t, s)
	start := make(chan struct{})
	out := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := expectedBatchRows(t, BatchWrite{Kind: BatchWriteUpdate, Table: "notes", Filter: "title = 'job' AND score = 7", Set: map[string]any{"score": 8}}, 1)
			_, err := s.Batch(context.Background(), "b", []BatchWrite{w, {Kind: BatchWriteInsert, Table: "notes", Records: []map[string]any{{"title": "event"}}}}, BatchOpts{}, Embedder{}, nil, Incarnation{})
			out <- err
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	wins, conflicts := 0, 0
	for err := range out {
		if err == nil {
			wins++
		} else if errors.Is(err, derr.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 || batchConditionCount(t, s, "title = 'event'") != 1 {
		t.Fatalf("claims: wins=%d conflicts=%d, events must equal one", wins, conflicts)
	}
	t.Logf("real SQLite claims: winners=%d conflicts=%d committed events=1", wins, conflicts)
}

func TestBatchExpectedRowsHashDistinguishesPresenceAndSecrets(t *testing.T) {
	for _, secretFields := range []bool{false, true} {
		t.Run(fmt.Sprint(secretFields), func(t *testing.T) {
			fields := []schema.Field{{Name: "score", Type: schema.Number}}
			keys := testKeyring(t, 1)
			if secretFields {
				fields = append(fields, schema.Field{Name: "token", Type: schema.Secret})
			}
			schemas := map[string]*schema.TableSchema{"notes": {Fields: fields}}
			w := BatchWrite{Kind: BatchWriteUpdate, Table: "notes", Filter: "score = ?", Args: []any{7}, Set: map[string]any{"score": 8}}
			old, err := BatchPayloadHash(t.Context(), keys, schemas, []BatchWrite{w}, BatchOpts{})
			if err != nil {
				t.Fatal(err)
			}
			zero, err := BatchPayloadHash(t.Context(), keys, schemas, []BatchWrite{expectedBatchRows(t, w, 0)}, BatchOpts{})
			if err != nil {
				t.Fatal(err)
			}
			one, err := BatchPayloadHash(t.Context(), keys, schemas, []BatchWrite{expectedBatchRows(t, w, 1)}, BatchOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if old.Primary == zero.Primary || zero.Primary == one.Primary || old.Primary == one.Primary {
				t.Fatal("omitted, zero and one must fingerprint differently, including the secret canonicalization path")
			}
		})
	}
}

func TestBatchExpectedRowsPreservesLegacyHashWhenOmitted(t *testing.T) {
	w := BatchWrite{Kind: BatchWriteUpdate, Table: "notes", Filter: "score = ?", Args: []any{7}, Set: map[string]any{"score": 8}}
	hash, err := BatchPayloadHash(t.Context(), nil, map[string]*schema.TableSchema{"notes": {Fields: noteFields()}}, []BatchWrite{w}, BatchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if hash.Primary != "123c8e5b34491a4f1159a07854f87382d7bc6340bd7e594e7f75310e19ffdf96" {
		t.Fatalf("omission changed the legacy payload hash: %s", hash.Primary)
	}
}

func TestBatchExpectedRowsCountsOnlyVisibleRows(t *testing.T) {
	s := openBatchStore(t)
	if _, err := s.CreateTable(t.Context(), "b", "owned", []schema.Field{{Name: "score", Type: schema.Number}}, TableOpts{RowAccess: "own"}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(t.Context(), "b", "owned", []map[string]any{{"score": 7}}, WriteOpts{Owner: "bob"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	w := BatchWrite{Kind: BatchWriteDelete, Table: "owned", Filter: "1=1", Scope: &RowScope{Owner: "alice"}}
	if _, err := batchConditionRun(t, s, []BatchWrite{expectedBatchRows(t, w, 0)}, BatchOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := batchConditionRun(t, s, []BatchWrite{expectedBatchRows(t, w, 1)}, BatchOpts{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("hidden row satisfied condition: %v", err)
	}
	rows, err := s.Query(t.Context(), "b", "SELECT count(*) AS n FROM owned", nil, [16]byte{}, Page{})
	if err != nil || rows.Rows[0]["n"] != int64(1) {
		t.Fatalf("hidden row changed: %+v %v", rows, err)
	}
}

func TestBatchExpectedRowsDoesNotBypassRowBudget(t *testing.T) {
	s := openBatchStore(t)
	rows := make([]map[string]any, 1000)
	for i := range rows {
		rows[i] = map[string]any{"title": fmt.Sprint(i), "score": 7}
	}
	if _, err := s.Insert(t.Context(), "b", "notes", rows, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	batchConditionSeed(t, s)
	w := expectedBatchRows(t, BatchWrite{Kind: BatchWriteUpdate, Table: "notes", Filter: "score = 7", Set: map[string]any{"score": 8}}, 1000)
	if _, err := batchConditionRun(t, s, []BatchWrite{w}, BatchOpts{}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("row budget must still win: %v", err)
	}
	if n := batchConditionCount(t, s, "score = 7"); n != 1001 {
		t.Fatalf("budget rollback left %d original rows", n)
	}
}
